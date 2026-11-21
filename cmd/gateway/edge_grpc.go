package main

// gRPC server for the simulator and the ground receivers (proto/atc.proto,
// service Gateway). It feeds the same code as the HTTP endpoints
// POST /api/positions and POST /api/observations.
//
// With MTLS on (the default): only the certificate named "simulator" may
// report positions, only "receiver-*" certificates may report observations,
// and a receiver may only report under its own name. The HTTP input
// endpoints are refused at the same time (httpInput).

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"atc/internal/atcpb"
	"atc/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type edgeServer struct {
	atcpb.UnimplementedGatewayServer
	g *gateway
	// checkIdentity: a receiver's certificate must match its receiver_id.
	checkIdentity bool
}

// edgeRules: who may call each Gateway RPC when mTLS is on.
var edgeRules = map[string]mtls.Rule{
	atcpb.Gateway_ReportPositions_FullMethodName:    mtls.Only("simulator"),
	atcpb.Gateway_ReportObservations_FullMethodName: func(name string) bool { return strings.HasPrefix(name, "receiver-") },
}

func (g *gateway) serveGRPC(addr string, withMTLS bool) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("grpc listen %s: %v", addr, err)
	}
	var opts []grpc.ServerOption
	if withMTLS {
		if opts, err = mtls.ServerOptions("gateway", edgeRules); err != nil {
			log.Fatalf("mtls: %v", err)
		}
	}
	log.Printf("gateway gRPC (simulator, receivers) on %s, mutual TLS %v", addr, withMTLS)
	log.Fatal(g.newEdgeServer(withMTLS, opts...).Serve(lis))
}

func (g *gateway) newEdgeServer(checkIdentity bool, opts ...grpc.ServerOption) *grpc.Server {
	gs := grpc.NewServer(opts...)
	atcpb.RegisterGatewayServer(gs, &edgeServer{g: g, checkIdentity: checkIdentity})
	return gs
}

// httpInput guards an HTTP endpoint that takes input from the simulator or
// the receivers. With mTLS on, that input must come over gRPC with the
// sender's certificate, so the unauthenticated HTTP path is closed.
func httpInput(h http.HandlerFunc) http.HandlerFunc {
	if !mtls.Enabled() {
		return h
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusForbidden, "input needs gRPC with mutual TLS (MTLS=on)")
	}
}

func (e *edgeServer) ReportPositions(_ context.Context, b *atcpb.PositionBatch) (*atcpb.Instructions, error) {
	var seen []string
	for _, p := range b.Positions {
		if p.Callsign == "" {
			continue
		}
		// Only what the aircraft claims; sector, owner, status and verify are
		// the gateway's to decide.
		e.g.accept(position{Callsign: p.Callsign, Seq: p.Seq, X: p.XNm, Y: p.YNm, Alt: p.AltFt, Heading: p.HeadingDeg, Speed: p.SpeedKt})
		seen = append(seen, p.Callsign)
	}
	// The reply carries the committed clearances to fly (conflict_follow.go).
	return instructionsPB(e.g.instructionsFor(seen)), nil
}

func (e *edgeServer) ReportObservations(ctx context.Context, b *atcpb.ObservationBatch) (*atcpb.Empty, error) {
	if e.checkIdentity {
		name, err := mtls.PeerName(ctx)
		if err != nil || name != b.ReceiverId {
			return nil, status.Errorf(codes.PermissionDenied, "certificate %q may not report as %q", name, b.ReceiverId)
		}
	}
	batch := observationBatch{ReceiverID: b.ReceiverId, Observations: make([]observation, 0, len(b.Observations))}
	for _, o := range b.Observations {
		batch.Observations = append(batch.Observations, observation{Callsign: o.Callsign, Seq: o.Seq, RangeNM: o.RangeNm})
	}
	g := e.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ver == nil {
		return nil, status.Error(codes.FailedPrecondition, "verification is off (RECEIVERS not set)")
	}
	if err := g.ver.observe(batch, time.Now()); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &atcpb.Empty{}, nil
}
