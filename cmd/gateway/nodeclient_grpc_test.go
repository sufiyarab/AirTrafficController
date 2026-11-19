package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"atc/internal/atcpb"
	"atc/internal/fsm"

	"google.golang.org/grpc"
)

// fakeNode answers Apply according to the command's key, so one server can
// play leader, follower and a refusing state machine.
type fakeNode struct {
	atcpb.UnimplementedNodeServer
	tracked []*atcpb.Position
}

func (f *fakeNode) Apply(_ context.Context, c *atcpb.Command) (*atcpb.ApplyReply, error) {
	switch c.Key {
	case "follower":
		return &atcpb.ApplyReply{NotLeader: true, LeaderId: "sector-1-b", Error: "not leader"}, nil
	case "refused":
		return &atcpb.ApplyReply{Rejected: true, Error: "already owned"}, nil
	case "timeout":
		return &atcpb.ApplyReply{Error: "timed out enqueuing operation"}, nil
	}
	return &atcpb.ApplyReply{Index: 42, ResultJson: []byte(`{"callsign":"ATC001","status":"ACTIVE","sector":"sector-1","index":42}`)}, nil
}

func (f *fakeNode) Track(_ context.Context, b *atcpb.PositionBatch) (*atcpb.Empty, error) {
	f.tracked = append(f.tracked, b.Positions...)
	return &atcpb.Empty{}, nil
}

func (f *fakeNode) Status(context.Context, *atcpb.Empty) (*atcpb.NodeStatus, error) {
	return &atcpb.NodeStatus{Id: "sector-1-a", Cluster: "sector-1", State: "Leader", LeaderId: "sector-1-a", Term: "3"}, nil
}

// startFake serves fakeNode on localhost and returns a grpcNodes client for
// it. The node ID is "127.0.0.1", since the client dials <id>:<port>.
func startFake(t *testing.T) (*fakeNode, *grpcNodes) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNode{}
	gs := grpc.NewServer()
	atcpb.RegisterNodeServer(gs, f)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	return f, newGRPCNodes(port, nil)
}

// The gRPC client must report the same error types as the HTTP client, since
// the handoff and clearance code branches on them.
func TestGRPCNodesApplyErrors(t *testing.T) {
	_, n := startFake(t)
	const id = "127.0.0.1"

	rep, err := n.Apply(id, fsm.Command{Op: "claim", Key: "ATC001"})
	if err != nil || rep.Index != 42 {
		t.Fatalf("ok apply = %+v, %v", rep, err)
	}
	var o fsm.Owner
	if err := json.Unmarshal(rep.Result, &o); err != nil || o.Sector != "sector-1" {
		t.Fatalf("result = %s, %v", rep.Result, err)
	}

	_, err = n.Apply(id, fsm.Command{Key: "follower"})
	var nl *notLeaderError
	if !errors.As(err, &nl) || nl.LeaderID != "sector-1-b" {
		t.Fatalf("follower: %v", err)
	}

	_, err = n.Apply(id, fsm.Command{Key: "refused"})
	var rej *rejectedError
	if !errors.As(err, &rej) || rej.msg != "already owned" {
		t.Fatalf("refused: %v", err)
	}

	_, err = n.Apply(id, fsm.Command{Key: "timeout"})
	if err == nil || errors.As(err, &rej) || errors.As(err, &nl) {
		t.Fatalf("timeout should be a plain error, got %v", err)
	}
}

func TestGRPCNodesTrackAndStatus(t *testing.T) {
	f, n := startFake(t)
	const id = "127.0.0.1"
	batch := []position{{Callsign: "ATC001", Seq: 7, X: 1, Y: 2, Alt: 3, Heading: 4, Speed: 5, Sector: "sector-1", Owner: "sector-1", Status: "ACTIVE", Verify: "VERIFIED"}}
	if err := n.Track(id, batch); err != nil {
		t.Fatal(err)
	}
	if len(f.tracked) != 1 || f.tracked[0].Seq != 7 || f.tracked[0].XNm != 1 || f.tracked[0].Verify != "VERIFIED" {
		t.Fatalf("tracked = %+v", f.tracked)
	}
	st, err := n.Status(id)
	if err != nil || st.State != "Leader" || st.LeaderID != "sector-1-a" {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

// An unreachable node is an error, not a hang: the gateway marks it down.
func TestGRPCNodesUnreachable(t *testing.T) {
	n := newGRPCNodes("1", nil) // nothing listens on port 1
	if _, err := n.Status("127.0.0.1"); err == nil {
		t.Fatal("expected an error")
	}
}
