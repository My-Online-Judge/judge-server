package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
)

// TokenHeader is the metadata key of the optional service token. Its value is
// the hex SHA-256 of the configured TOKEN, the digest judge-worker computes.
const TokenHeader = "x-judge-server-token"

// TLSConfig builds the server's TLS config from dir: its certificate and key from
// tls.crt and tls.key, and the CA that must have signed every client certificate
// from ca.crt. A client without a certificate, or with one that CA did not sign,
// fails the handshake; TLS below 1.2 is refused. Any missing or unusable file is
// an error: the server never falls back to plaintext or to an unverified client.
func TLSConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("load server certificate from %s: %w", dir, err)
	}
	pool, err := loadCAPool(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return pool, nil
}

// tokenDigest is the value clients put in TokenHeader for token: its hex
// SHA-256, or "" when no token is configured (the check is then off).
func tokenDigest(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AuthInterceptor admits a call only when it carries the service token and comes
// from an allowed client. Both checks run on top of the TLS handshake, which has
// already proved the peer holds a certificate signed by the CA.
//
//   - tokenSHA256Hex != "": the call must carry TokenHeader with exactly that
//     value, else UNAUTHENTICATED. "" turns the token check off.
//   - The CN of the peer's verified leaf certificate must be in allowedCNs, else
//     PERMISSION_DENIED, except for Ping, which any CA-signed client may call
//     (the healthcheck dials with the server's own certificate). The token is
//     still required for Ping. An empty allowedCNs therefore refuses every call
//     but Ping; Serve refuses to start with one.
func AuthInterceptor(allowedCNs []string, tokenSHA256Hex string) grpc.UnaryServerInterceptor {
	allowed := make(map[string]struct{}, len(allowedCNs))
	for _, cn := range allowedCNs {
		if cn != "" { // a blank entry must never admit a certificate with an empty CN
			allowed[cn] = struct{}{}
		}
	}
	want := []byte(tokenSHA256Hex)

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if len(want) > 0 && !hasToken(ctx, want) {
			cn, _ := peerCN(ctx) // for the log only
			return nil, rejectAuth(info.FullMethod, cn, status.Error(codes.Unauthenticated, "missing or wrong "+TokenHeader))
		}
		if info.FullMethod != judgev1.JudgeServer_Ping_FullMethodName {
			cn, ok := peerCN(ctx)
			if _, in := allowed[cn]; !ok || !in {
				return nil, rejectAuth(info.FullMethod, cn, status.Error(codes.PermissionDenied, "client certificate CN is not allowed"))
			}
		}
		return handler(ctx, req)
	}
}

// hasToken reports whether one of the call's token values equals want, comparing
// in constant time.
func hasToken(ctx context.Context, want []byte) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	found := false
	for _, v := range md.Get(TokenHeader) {
		if subtle.ConstantTimeCompare([]byte(v), want) == 1 {
			found = true
		}
	}
	return found
}

// peerCN returns the common name of the leaf of the peer's verified certificate
// chain. ok is false for a plaintext peer or one with no verified chain.
func peerCN(ctx context.Context) (cn string, ok bool) {
	p, found := peer.FromContext(ctx)
	if !found {
		return "", false
	}
	info, isTLS := p.AuthInfo.(credentials.TLSInfo)
	if !isTLS || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return "", false
	}
	return info.State.VerifiedChains[0][0].Subject.CommonName, true
}

// rejectAuth logs a refused call and returns the status error for the client.
func rejectAuth(method, cn string, err error) error {
	slog.Warn("request rejected", "method", method, "cn", cn, "code", status.Code(err).String(), "reason", status.Convert(err).Message())
	return err
}
