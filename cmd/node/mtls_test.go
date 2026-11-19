package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"atc/internal/atcpb"
	"atc/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
)

// The mTLS demo as a test: a node with mutual TLS on, and four callers.
func TestNodeMTLS(t *testing.T) {
	dir := t.TempDir()
	if err := mtls.WriteTestCerts(dir, "127.0.0.1", "gateway", "receiver-a"); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // a second, unrelated CA: what a rogue container could make itself
	if err := mtls.WriteTestCerts(other, "gateway"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CERT_DIR", dir)

	s, _ := singleNode(t)
	opts, err := mtls.ServerOptions("127.0.0.1", nodeRules)
	if err != nil {
		t.Fatal(err)
	}
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	gs := newGRPCServer(s, opts...)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	dial := func(creds credentials.TransportCredentials) atcpb.NodeClient {
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return atcpb.NewNodeClient(conn)
	}
	as := func(name, certDir string) atcpb.NodeClient {
		t.Setenv("CERT_DIR", certDir)
		creds, err := mtls.ClientCreds(name)
		if err != nil {
			t.Fatal(err)
		}
		return dial(creds)
	}
	write := &atcpb.Command{Op: "claim", Key: "ATC001", Value: []byte(`{"to":"sector-1"}`)}

	// 1. No certificate at all (plain gRPC): refused before any RPC runs.
	if _, err := dial(insecure.NewCredentials()).Apply(ctx(t), write); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("no certificate: want Unavailable (handshake fails), got %v", err)
	}
	// 2. A certificate called "gateway", but from someone else's CA: refused.
	if _, err := as("gateway", other).Apply(ctx(t), write); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("foreign CA: want Unavailable (handshake fails), got %v", err)
	}
	// 3. A valid certificate for the wrong service: may read, may not write.
	rx := as("receiver-a", dir)
	if _, err := rx.Status(ctx(t), &atcpb.Empty{}); err != nil {
		t.Fatalf("receiver-a reading status: %v", err)
	}
	if _, err := rx.Apply(ctx(t), write); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("receiver-a writing: want PermissionDenied, got %v", err)
	}
	if _, err := rx.Track(ctx(t), &atcpb.PositionBatch{}); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Fatalf("receiver-a tracking: want PermissionDenied, got %v", err)
	}
	// 4. The gateway: allowed.
	if rep, err := as("gateway", dir).Apply(ctx(t), write); err != nil || rep.Error != "" {
		t.Fatalf("gateway writing: %+v, %v", rep, err)
	}
}

// With mTLS on, the HTTP write endpoints are closed; reads stay open.
func TestHTTPWritesClosedWithMTLS(t *testing.T) {
	t.Setenv("MTLS", "on")
	rec := httptest.NewRecorder()
	httpWrite(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })(rec, httptest.NewRequest("POST", "/apply", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MTLS=on: got %d", rec.Code)
	}
	t.Setenv("MTLS", "off")
	rec = httptest.NewRecorder()
	httpWrite(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })(rec, httptest.NewRequest("POST", "/apply", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("MTLS=off: got %d", rec.Code)
	}
}
