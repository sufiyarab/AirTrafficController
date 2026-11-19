package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"atc/internal/atcpb"
	"atc/internal/fsm"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// singleNode starts a one-replica Raft cluster in memory (it elects itself)
// and returns its server with a gRPC client connected to it.
func singleNode(t *testing.T) (*server, atcpb.NodeClient) {
	t.Helper()
	cfg := raft.DefaultConfig()
	cfg.LocalID = "n1"
	cfg.HeartbeatTimeout, cfg.ElectionTimeout, cfg.LeaderLeaseTimeout = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	cfg.LogOutput = &strings.Builder{}
	store := raft.NewInmemStore()
	addr, trans := raft.NewInmemTransport("")
	state := fsm.New()
	r, err := raft.NewRaft(cfg, state, store, store, raft.NewInmemSnapshotStore(), trans)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Shutdown() })
	r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{ID: "n1", Address: addr}}})
	for i := 0; r.State() != raft.Leader; i++ {
		if i > 100 {
			t.Fatal("no leader")
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := &server{id: "n1", cluster: "sector-1", raft: r, fsm: state, tracks: map[string]track{}}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := newGRPCServer(s)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return s, atcpb.NewNodeClient(conn)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestGRPCApplyOwnersStatus(t *testing.T) {
	_, c := singleNode(t)

	st, err := c.Status(ctx(t), &atcpb.Empty{})
	if err != nil || st.State != "Leader" || st.LeaderId != "n1" || st.Cluster != "sector-1" {
		t.Fatalf("status = %+v, %v", st, err)
	}

	claim := &atcpb.Command{Op: "claim", Key: "ATC001", Value: []byte(`{"to":"sector-1"}`)}
	rep, err := c.Apply(ctx(t), claim)
	if err != nil || rep.Error != "" || rep.Index == 0 {
		t.Fatalf("claim = %+v, %v", rep, err)
	}
	var o fsm.Owner
	if err := json.Unmarshal(rep.ResultJson, &o); err != nil || o.Sector != "sector-1" || o.Status != fsm.Active {
		t.Fatalf("claim result = %s (%v)", rep.ResultJson, err)
	}

	// The same claim again is committed but refused by the state machine.
	rep, err = c.Apply(ctx(t), claim)
	if err != nil || !rep.Rejected || !strings.Contains(rep.Error, "already owned") {
		t.Fatalf("second claim = %+v, %v", rep, err)
	}

	tab, err := c.Owners(ctx(t), &atcpb.Empty{})
	if err != nil || tab.Owners["ATC001"].GetSector() != "sector-1" {
		t.Fatalf("owners = %+v, %v", tab, err)
	}
}

func TestGRPCNotLeader(t *testing.T) {
	s, c := singleNode(t)
	if err := s.raft.Shutdown().Error(); err != nil { // a shut-down node is not the leader
		t.Fatal(err)
	}
	rep, err := c.Apply(ctx(t), &atcpb.Command{Op: "set", Key: "k", Value: []byte(`1`)})
	if err != nil || !rep.NotLeader {
		t.Fatalf("apply on non-leader = %+v, %v", rep, err)
	}
}

// Track over gRPC must store the same JSON report as Track over HTTP, and
// apply the same FIFO rule (an older seq is dropped).
func TestGRPCTrackMatchesHTTP(t *testing.T) {
	s, c := singleNode(t)
	p := &atcpb.Position{Callsign: "ATC001", Seq: 5, XNm: 10, YNm: 20, AltFt: 30000, HeadingDeg: 90, SpeedKt: 450, Sector: "sector-1", Owner: "sector-1", Status: "ACTIVE"}
	if _, err := c.Track(ctx(t), &atcpb.PositionBatch{Positions: []*atcpb.Position{p}}); err != nil {
		t.Fatal(err)
	}
	viaGRPC := string(s.tracks["ATC001"].Report)

	// The same report, as the gateway's HTTP transport would send it.
	body := `[{"callsign":"ATC002","seq":5,"x":10,"y":20,"alt":30000,"heading":90,"speed":450,"sector":"sector-1","owner":"sector-1","status":"ACTIVE"}]`
	s.track(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/track", strings.NewReader(body)))
	var a, b map[string]any
	json.Unmarshal([]byte(viaGRPC), &a)
	json.Unmarshal(s.tracks["ATC002"].Report, &b)
	b["callsign"] = "ATC001"
	if ja, jb := mustJSON(a), mustJSON(b); ja != jb {
		t.Fatalf("gRPC report %s != HTTP report %s", ja, jb)
	}

	p.Seq = 4 // older than what was delivered
	c.Track(ctx(t), &atcpb.PositionBatch{Positions: []*atcpb.Position{p}})
	if s.tracks["ATC001"].Seq != 5 || s.dropped != 1 {
		t.Fatalf("stale report not dropped: seq %d, dropped %d", s.tracks["ATC001"].Seq, s.dropped)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
