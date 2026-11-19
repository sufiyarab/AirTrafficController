// Command intruder is the mutual-TLS demo: a rogue container on the atc
// network that tries to write to the airspace registry's Raft log, the
// single source of truth for who controls each flight.
//
//	docker compose run --rm intruder            # with mutual TLS (default)
//	MTLS=off docker compose up -d               # the "before" picture:
//	MTLS=off docker compose run --rm intruder   # the same attempts succeed
//
// It makes four attempts on the registry leader and prints the outcome of each:
//
//  1. plain HTTP POST /apply (the original endpoint, no authentication)
//  2. gRPC Apply without a certificate
//  3. gRPC Apply with a certificate named "gateway" from its own, home-made CA
//  4. gRPC Apply with a real certificate for another service ("receiver-a",
//     as if that container were compromised), plus a read (Status) to show
//     the certificate itself is accepted
//
// The write is harmless: op "set" on key intruder/<time>.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"atc/internal/atcpb"
	"atc/internal/mtls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	gateway := env("GATEWAY", "http://gateway:8080")
	stolen := env("AS", "receiver-a")
	leader := registryLeader(gateway)
	key := fmt.Sprintf("intruder/%d", time.Now().Unix())
	fmt.Printf("target: registry leader %s; writing key %q\n\n", leader, key)

	// 1. HTTP
	body, _ := json.Marshal(map[string]any{"op": "set", "key": key, "value": json.RawMessage(`"http"`)})
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Post("http://"+leader+":8080/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		report("1. HTTP POST /apply, no credentials", false, err.Error())
	} else {
		var r map[string]any
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		report("1. HTTP POST /apply, no credentials", resp.StatusCode == http.StatusOK, fmt.Sprintf("HTTP %d %v", resp.StatusCode, r))
	}

	// 2. gRPC, no certificate
	if try("2. gRPC Apply, no certificate", leader, insecure.NewCredentials(), key, false) {
		fmt.Println("\nThe node accepts plain connections (MTLS=off), so certificates do not matter: done.")
		return
	}

	// 3. gRPC, a "gateway" certificate from a CA the intruder made itself. It
	// trusts the real CA (ca.crt is public), so it is the node that refuses.
	realDir := env("REAL_CERT_DIR", "/certs")
	home, _ := os.MkdirTemp("", "intruder")
	if err := mtls.WriteTestCerts(home, "gateway"); err == nil {
		fake, err1 := tls.LoadX509KeyPair(filepath.Join(home, "gateway.crt"), filepath.Join(home, "gateway.key"))
		caPEM, err2 := os.ReadFile(filepath.Join(realDir, "ca.crt"))
		if err1 == nil && err2 == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(caPEM)
			creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{fake}, RootCAs: pool, MinVersion: tls.VersionTLS13})
			try(`3. gRPC Apply, forged "gateway" cert (own CA)`, leader, creds, key, false)
		}
	}

	// 4. gRPC, a genuine certificate for another service
	os.Setenv("CERT_DIR", realDir)
	creds, err := mtls.ClientCreds(stolen)
	if err != nil {
		report(fmt.Sprintf("4. gRPC Apply, real %q certificate", stolen), false, "no certificate to use: "+err.Error())
		return
	}
	try(fmt.Sprintf("4. gRPC Apply, real %q certificate", stolen), leader, creds, key, true)
}

// try dials the leader's gRPC port with creds and attempts one write. It
// reports whether the write was committed.
func try(label, leader string, creds credentials.TransportCredentials, key string, alsoRead bool) bool {
	conn, err := grpc.NewClient(leader+":"+env("NODE_GRPC_PORT", "9090"), grpc.WithTransportCredentials(creds))
	if err != nil {
		report(label, false, err.Error())
		return false
	}
	defer conn.Close()
	c := atcpb.NewNodeClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if alsoRead {
		if st, err := c.Status(ctx, &atcpb.Empty{}); err == nil {
			defer fmt.Printf("   (its certificate is valid: reading Status is allowed, %s is %s)\n", st.Id, st.State)
		}
	}
	rep, err := c.Apply(ctx, &atcpb.Command{Op: "set", Key: key, Value: []byte(`"grpc"`)})
	switch {
	case err != nil:
		s := status.Convert(err)
		report(label, false, fmt.Sprintf("%s: %s", s.Code(), short(s.Message())))
	case rep.Error != "":
		report(label, false, rep.Error)
	default:
		report(label, true, fmt.Sprintf("committed at log index %d", rep.Index))
		return true
	}
	return false
}

func report(label string, wrote bool, detail string) {
	verdict := "REFUSED "
	if wrote {
		verdict = "WROTE TO THE REGISTRY"
	}
	fmt.Printf("%-48s %s\n   %s\n", label, verdict, detail)
}

func short(s string) string {
	if i := strings.Index(s, "desc = "); i >= 0 {
		s = s[i+7:]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// registryLeader asks the gateway (its read-only state is public, like the
// radar) which node leads the registry.
func registryLeader(gateway string) string {
	for range 30 {
		resp, err := http.Get(gateway + "/api/state")
		if err == nil {
			var st struct {
				Ready map[string]struct {
					Leader string `json:"leader"`
				} `json:"ready"`
			}
			json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			if l := st.Ready["registry"].Leader; l != "" {
				return l
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintln(os.Stderr, "no registry leader known to the gateway")
	os.Exit(1)
	return ""
}
