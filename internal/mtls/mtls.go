// Package mtls is the mutual-TLS setup shared by the nodes and the gateway.
//
// Every service has a certificate signed by one dev CA (scripts/gen-certs.sh),
// with the service name as its DNS SAN: "gateway", "simulator",
// "receiver-a", "sector-1-a", ... A gRPC server built with ServerOptions
//
//   - rejects any client without a certificate from that CA (TLS handshake),
//   - then lets each RPC through only if the caller's name passes that RPC's
//     rule (Authorize). An RPC without a rule is refused.
//
// Configuration: MTLS=on|off (default on), CERT_DIR (default /certs).
package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Enabled reports whether MTLS is on (the default).
func Enabled() bool {
	switch os.Getenv("MTLS") {
	case "", "on", "true", "1":
		return true
	}
	return false
}

func certDir() string {
	if d := os.Getenv("CERT_DIR"); d != "" {
		return d
	}
	return "/certs"
}

// load reads <dir>/<name>.crt, <name>.key and the CA certificate.
func load(name string) (tls.Certificate, *x509.CertPool, error) {
	dir := certDir()
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key"))
	if err != nil {
		return cert, nil, fmt.Errorf("certificate for %s: %w", name, err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return cert, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return cert, nil, errors.New("ca.crt: no certificate found")
	}
	return cert, pool, nil
}

// ServerTLS is the server side: present our certificate, require and verify
// the client's.
func ServerTLS(name string) (*tls.Config, error) {
	cert, pool, err := load(name)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientCreds is the client side: present our certificate and verify that
// the server's is signed by the CA and names the host we dialled.
func ClientCreds(name string) (credentials.TransportCredentials, error) {
	cert, pool, err := load(name)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// Rule decides whether the caller named name may call one RPC.
type Rule func(name string) bool

// AnyService lets every holder of a valid certificate through.
func AnyService(string) bool { return true }

// Only lets exactly one service through.
func Only(want string) Rule { return func(name string) bool { return name == want } }

// ServerOptions returns the TLS credentials and the authorization check for a
// gRPC server run by service name. rules maps a full method name
// ("/atc.Node/Apply") to who may call it.
func ServerOptions(name string, rules map[string]Rule) ([]grpc.ServerOption, error) {
	cfg, err := ServerTLS(name)
	if err != nil {
		return nil, err
	}
	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.UnaryInterceptor(Authorize(rules)),
	}, nil
}

// Authorize refuses an RPC unless its rule accepts the caller's certificate
// name. Methods with no rule are refused, so a new RPC is closed by default.
func Authorize(rules map[string]Rule) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		name, err := PeerName(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		rule, ok := rules[info.FullMethod]
		if !ok || !rule(name) {
			return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s", name, info.FullMethod)
		}
		return h(ctx, req)
	}
}

// PeerName is the service name in the caller's verified certificate (its
// first DNS SAN), or an error if the connection has no verified certificate.
func PeerName(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 || len(ti.State.VerifiedChains[0]) == 0 {
		return "", errors.New("no verified client certificate")
	}
	leaf := ti.State.VerifiedChains[0][0]
	if len(leaf.DNSNames) == 0 {
		return "", errors.New("client certificate has no DNS name")
	}
	return leaf.DNSNames[0], nil
}
