package main

// gRPC implementation of nodeClient (proto/atc.proto, service Node).
// Selected with TRANSPORT=grpc (the default); TRANSPORT=http keeps the
// original HTTP/JSON transport for comparison experiments.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"atc/internal/atcpb"
	"atc/internal/fsm"
	"atc/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const rpcTimeout = time.Second // same as the HTTP client's timeout

type grpcNodes struct {
	port  string
	creds credentials.TransportCredentials

	mu    sync.Mutex
	conns map[string]atcpb.NodeClient // node ID -> client on one shared connection
}

func newGRPCNodes(port string, creds credentials.TransportCredentials) *grpcNodes {
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	return &grpcNodes{port: port, creds: creds, conns: map[string]atcpb.NodeClient{}}
}

// client returns the stub for node id, creating its connection on first use.
// The connection is lazy and reconnects by itself after the node restarts.
func (g *grpcNodes) client(id string) (atcpb.NodeClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.conns[id]; ok {
		return c, nil
	}
	// passthrough: resolve the container name on every connection attempt, so
	// a restarted container with a new IP is found at once (the default dns
	// resolver caches for 30 s). Backoff capped at 1 s, not gRPC's default
	// 120 s, so a node is usable again right after `docker start`.
	conn, err := grpc.NewClient("passthrough:///"+id+":"+g.port,
		grpc.WithTransportCredentials(g.creds),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: time.Second},
			MinConnectTimeout: time.Second,
		}))
	if err != nil {
		return nil, err
	}
	c := atcpb.NewNodeClient(conn)
	g.conns[id] = c
	return c, nil
}

func call[T any](g *grpcNodes, id string, f func(context.Context, atcpb.NodeClient) (T, error)) (T, error) {
	var zero T
	c, err := g.client(id)
	if err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return f(ctx, c)
}

func (g *grpcNodes) Status(id string) (nodeStatus, error) {
	r, err := call(g, id, func(ctx context.Context, c atcpb.NodeClient) (*atcpb.NodeStatus, error) {
		return c.Status(ctx, &atcpb.Empty{})
	})
	if err != nil {
		return nodeStatus{ID: id}, err
	}
	return nodeStatus{ID: r.Id, Cluster: r.Cluster, State: r.State, LeaderID: r.LeaderId, Term: r.Term, Commit: r.CommitIndex}, nil
}

func (g *grpcNodes) Apply(id string, cmd fsm.Command) (applyReply, error) {
	r, err := call(g, id, func(ctx context.Context, c atcpb.NodeClient) (*atcpb.ApplyReply, error) {
		return c.Apply(ctx, &atcpb.Command{Op: cmd.Op, Key: cmd.Key, Value: cmd.Value})
	})
	switch {
	case err != nil:
		return applyReply{}, err
	case r.NotLeader:
		return applyReply{}, &notLeaderError{r.LeaderId}
	case r.Rejected:
		return applyReply{}, &rejectedError{r.Error}
	case r.Error != "":
		return applyReply{}, errors.New(r.Error)
	}
	return applyReply{Index: r.Index, Result: r.ResultJson}, nil
}

func (g *grpcNodes) Track(id string, batch []position) error {
	b := &atcpb.PositionBatch{Positions: make([]*atcpb.Position, 0, len(batch))}
	for _, p := range batch {
		b.Positions = append(b.Positions, toPB(p))
	}
	_, err := call(g, id, func(ctx context.Context, c atcpb.NodeClient) (*atcpb.Empty, error) {
		return c.Track(ctx, b)
	})
	return err
}

func (g *grpcNodes) Owners(id string) (map[string]fsm.Owner, error) {
	r, err := call(g, id, func(ctx context.Context, c atcpb.NodeClient) (*atcpb.OwnerTable, error) {
		return c.Owners(ctx, &atcpb.Empty{})
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]fsm.Owner, len(r.Owners))
	for cs, o := range r.Owners {
		out[cs] = fsm.Owner{Callsign: o.Callsign, Status: o.Status, Sector: o.Sector, Target: o.Target, Index: o.Index}
	}
	return out, nil
}

func (g *grpcNodes) Sectors(id string) (fsm.SectorMap, bool, error) {
	var m fsm.SectorMap
	r, err := call(g, id, func(ctx context.Context, c atcpb.NodeClient) (*atcpb.JSONBlob, error) {
		return c.Sectors(ctx, &atcpb.Empty{})
	})
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(r.Json, &m); err != nil {
		return m, false, fmt.Errorf("sectors: %w", err)
	}
	return m, true, nil
}

func (g *grpcNodes) State(id string) (map[string]json.RawMessage, error) {
	r, err := call(g, id, func(ctx context.Context, c atcpb.NodeClient) (*atcpb.JSONBlob, error) {
		return c.State(ctx, &atcpb.Empty{})
	})
	if err != nil {
		return nil, err
	}
	var state map[string]json.RawMessage
	return state, json.Unmarshal(r.Json, &state)
}

func toPB(p position) *atcpb.Position {
	return &atcpb.Position{
		Callsign: p.Callsign, Seq: p.Seq, XNm: p.X, YNm: p.Y, AltFt: p.Alt, HeadingDeg: p.Heading, SpeedKt: p.Speed,
		Sector: p.Sector, Owner: p.Owner, Status: p.Status, Verify: p.Verify,
	}
}

// nodeClientFromEnv picks the transport: TRANSPORT=grpc (default) or http.
func nodeClientFromEnv(httpTimeout time.Duration) nodeClient {
	switch t := os.Getenv("TRANSPORT"); t {
	case "", "grpc":
		port := os.Getenv("NODE_GRPC_PORT")
		if port == "" {
			port = "9090"
		}
		var creds credentials.TransportCredentials
		if mtls.Enabled() {
			var err error
			if creds, err = mtls.ClientCreds("gateway"); err != nil {
				log.Fatalf("mtls: %v", err)
			}
		}
		log.Printf("gateway -> nodes: gRPC on port %s, mutual TLS %v", port, creds != nil)
		return newGRPCNodes(port, creds)
	case "http":
		if mtls.Enabled() {
			log.Fatal("TRANSPORT=http needs MTLS=off: with mutual TLS on, nodes only accept writes over gRPC")
		}
		log.Printf("gateway -> nodes: HTTP/JSON on port 8080")
		return newHTTPNodes(httpTimeout)
	default:
		log.Fatalf("TRANSPORT must be grpc or http, not %q", t)
		return nil
	}
}
