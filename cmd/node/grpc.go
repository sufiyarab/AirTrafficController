package main

// gRPC server for the Node service (proto/atc.proto). It runs next to the
// HTTP handlers and calls the same code (commit, deliver, currentStatus), so
// both transports behave identically.
//
// With MTLS on (the default) only clients with a certificate from the dev CA
// get past the TLS handshake, and only the gateway may write: Apply and Track
// accept the certificate named "gateway"; the read-only calls accept any
// service. The HTTP write endpoints are refused at the same time (main.go),
// so they are not a way around it.

import (
	"context"
	"encoding/json"
	"log"
	"net"

	"atc/internal/atcpb"
	"atc/internal/fsm"
	"atc/internal/mtls"

	"google.golang.org/grpc"
)

type grpcNode struct {
	atcpb.UnimplementedNodeServer
	s *server
}

func serveGRPC(s *server, addr string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("grpc listen %s: %v", addr, err)
	}
	var opts []grpc.ServerOption
	if mtls.Enabled() {
		if opts, err = mtls.ServerOptions(s.id, nodeRules); err != nil {
			log.Fatalf("mtls: %v", err)
		}
		log.Printf("grpc: mutual TLS on, writes only from the gateway")
	}
	log.Fatal(newGRPCServer(s, opts...).Serve(lis))
}

// nodeRules: who may call each Node RPC when mTLS is on.
var nodeRules = map[string]mtls.Rule{
	atcpb.Node_Apply_FullMethodName:   mtls.Only("gateway"),
	atcpb.Node_Track_FullMethodName:   mtls.Only("gateway"),
	atcpb.Node_Status_FullMethodName:  mtls.AnyService,
	atcpb.Node_Owners_FullMethodName:  mtls.AnyService,
	atcpb.Node_Sectors_FullMethodName: mtls.AnyService,
	atcpb.Node_State_FullMethodName:   mtls.AnyService,
}

func newGRPCServer(s *server, opts ...grpc.ServerOption) *grpc.Server {
	gs := grpc.NewServer(opts...)
	atcpb.RegisterNodeServer(gs, &grpcNode{s: s})
	return gs
}

func (n *grpcNode) Status(context.Context, *atcpb.Empty) (*atcpb.NodeStatus, error) {
	st := n.s.currentStatus()
	return &atcpb.NodeStatus{
		Id: st.ID, Cluster: st.Cluster, State: st.State, LeaderId: st.LeaderID,
		Term: st.Term, CommitIndex: st.CommitIndex,
	}, nil
}

func (n *grpcNode) Apply(_ context.Context, c *atcpb.Command) (*atcpb.ApplyReply, error) {
	r := n.s.commit(fsm.Command{Op: c.Op, Key: c.Key, Value: json.RawMessage(c.Value)})
	switch {
	case r.notLeader:
		return &atcpb.ApplyReply{NotLeader: true, LeaderId: r.leaderID, Error: "not leader"}, nil
	case r.err != nil:
		return &atcpb.ApplyReply{Error: r.err.Error()}, nil
	case r.rejected != nil:
		return &atcpb.ApplyReply{Index: r.index, Rejected: true, Error: r.rejected.Error()}, nil
	}
	var res []byte
	if r.result != nil {
		res, _ = json.Marshal(r.result)
	}
	return &atcpb.ApplyReply{Index: r.index, ResultJson: res}, nil
}

// trackReport is a position report in the same JSON shape the gateway sends
// over HTTP, so /tracks shows the same thing whichever transport delivered it.
type trackReport struct {
	Callsign string  `json:"callsign"`
	Seq      uint64  `json:"seq"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Alt      float64 `json:"alt"`
	Heading  float64 `json:"heading"`
	Speed    float64 `json:"speed"`
	Sector   string  `json:"sector"`
	Owner    string  `json:"owner"`
	Status   string  `json:"status"`
	Verify   string  `json:"verify,omitempty"`
}

func (n *grpcNode) Track(_ context.Context, b *atcpb.PositionBatch) (*atcpb.Empty, error) {
	batch := make([]json.RawMessage, 0, len(b.Positions))
	for _, p := range b.Positions {
		raw, _ := json.Marshal(trackReport{
			Callsign: p.Callsign, Seq: p.Seq, X: p.XNm, Y: p.YNm, Alt: p.AltFt,
			Heading: p.HeadingDeg, Speed: p.SpeedKt,
			Sector: p.Sector, Owner: p.Owner, Status: p.Status, Verify: p.Verify,
		})
		batch = append(batch, raw)
	}
	n.s.deliver(batch)
	return &atcpb.Empty{}, nil
}

func (n *grpcNode) Owners(context.Context, *atcpb.Empty) (*atcpb.OwnerTable, error) {
	t := &atcpb.OwnerTable{Owners: map[string]*atcpb.Owner{}}
	for cs, o := range n.s.fsm.Owners() {
		t.Owners[cs] = &atcpb.Owner{Callsign: o.Callsign, Status: o.Status, Sector: o.Sector, Target: o.Target, Index: o.Index}
	}
	return t, nil
}

func (n *grpcNode) Sectors(context.Context, *atcpb.Empty) (*atcpb.JSONBlob, error) {
	b, err := json.Marshal(n.s.fsm.Sectors())
	return &atcpb.JSONBlob{Json: b}, err
}

func (n *grpcNode) State(context.Context, *atcpb.Empty) (*atcpb.JSONBlob, error) {
	b, err := json.Marshal(n.s.fsm.All())
	return &atcpb.JSONBlob{Json: b}, err
}
