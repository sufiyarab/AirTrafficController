package main

import (
	"context"
	"net"
	"testing"
	"time"

	"atc/internal/atcpb"
	"atc/internal/fsm"
	"atc/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// edgeClient serves the gateway's gRPC service on localhost and returns a
// client for it, as the simulator and receivers would use it.
func edgeClient(t *testing.T, g *gateway) atcpb.GatewayClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := g.newEdgeServer(false)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return atcpb.NewGatewayClient(conn)
}

func edgeCtx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return c
}

// ReportPositions does what POST /api/positions does: record the claim, route
// it to its sector, and queue a claim for an aircraft nobody owns yet.
func TestEdgeReportPositions(t *testing.T) {
	g := newTestGateway()
	g.aircraft, g.outbox, g.owners, g.inflight = map[string]position{}, map[string]map[string]position{}, map[string]fsm.Owner{}, map[string]bool{}
	g.jobs = make(chan job, 4)
	g.geo.Store(fallbackGeo)
	c := edgeClient(t, g)

	// The client may not set sector/owner/status/verify: the gateway decides those.
	_, err := c.ReportPositions(edgeCtx(t), &atcpb.PositionBatch{Positions: []*atcpb.Position{
		{Callsign: "ATC001", Seq: 3, XNm: 20, YNm: 20, AltFt: 30000, HeadingDeg: 90, SpeedKt: 400, Owner: "forged", Verify: "VERIFIED"},
		{Callsign: ""}, // ignored
	}})
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.aircraft["ATC001"]
	if !ok || len(g.aircraft) != 1 || p.Seq != 3 || p.Alt != 30000 {
		t.Fatalf("aircraft = %+v", g.aircraft)
	}
	if p.Sector == "" || p.Owner != p.Sector || p.Verify != "" {
		t.Fatalf("gateway must decide sector/owner/verify itself, got %+v", p)
	}
	if j := <-g.jobs; j.op != "claim" || j.callsign != "ATC001" || j.to != p.Sector {
		t.Fatalf("job = %+v", j)
	}
}

func TestEdgeReportObservations(t *testing.T) {
	g := &gateway{ver: newVerifier(defaultVerifyConfig(), testReceivers())}
	c := edgeClient(t, g)

	_, err := c.ReportObservations(edgeCtx(t), &atcpb.ObservationBatch{ReceiverId: "receiver-a",
		Observations: []*atcpb.Observation{{Callsign: "ATC001", Seq: 1, RangeNm: 12.5}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ReportObservations(edgeCtx(t), &atcpb.ObservationBatch{ReceiverId: "nope"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown receiver: %v", err)
	}
	if g.ver.st.Observations != 1 {
		t.Fatalf("stats %+v", g.ver.st)
	}

	off := edgeClient(t, &gateway{}) // RECEIVERS not set
	if _, err := off.ReportObservations(edgeCtx(t), &atcpb.ObservationBatch{ReceiverId: "receiver-a"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("verification off: %v", err)
	}
}

// With mTLS: only "simulator" may report positions, and a receiver may only
// report under its own name.
func TestEdgeMTLSIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := mtls.WriteTestCerts(dir, "127.0.0.1", "gateway", "simulator", "receiver-a", "receiver-b"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CERT_DIR", dir)
	g := newTestGateway()
	g.aircraft, g.outbox, g.owners, g.inflight = map[string]position{}, map[string]map[string]position{}, map[string]fsm.Owner{}, map[string]bool{}
	g.jobs = make(chan job, 4)
	g.geo.Store(fallbackGeo)
	g.ver = newVerifier(defaultVerifyConfig(), testReceivers())

	// The test dials 127.0.0.1, so the server presents that certificate; in
	// compose it is the one named "gateway".
	opts, err := mtls.ServerOptions("127.0.0.1", edgeRules)
	if err != nil {
		t.Fatal(err)
	}
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	gs := g.newEdgeServer(true, opts...)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	as := func(name string) atcpb.GatewayClient {
		creds, err := mtls.ClientCreds(name)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return atcpb.NewGatewayClient(conn)
	}
	pos := &atcpb.PositionBatch{Positions: []*atcpb.Position{{Callsign: "GHOST99", Seq: 1, XNm: 50, YNm: 50, AltFt: 20000}}}
	obs := func(id string) *atcpb.ObservationBatch { return &atcpb.ObservationBatch{ReceiverId: id} }

	if _, err := as("receiver-a").ReportPositions(edgeCtx(t), pos); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("receiver injecting positions: %v", err)
	}
	if _, err := as("simulator").ReportPositions(edgeCtx(t), pos); err != nil {
		t.Fatalf("simulator: %v", err)
	}
	if _, err := as("simulator").ReportObservations(edgeCtx(t), obs("receiver-a")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("simulator posing as a receiver: %v", err)
	}
	if _, err := as("receiver-a").ReportObservations(edgeCtx(t), obs("receiver-b")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("receiver-a posing as receiver-b: %v", err)
	}
	if _, err := as("receiver-a").ReportObservations(edgeCtx(t), obs("receiver-a")); err != nil {
		t.Fatalf("receiver-a as itself: %v", err)
	}
}
