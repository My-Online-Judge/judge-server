package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/judge"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

func TestAuthTokenMissing(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}})
	c := r.client(t, testutil.WorkerCN)

	for _, tc := range []struct {
		name   string
		values []string // the token metadata the client sends
	}{
		{"missing", nil},
		{"wrong digest", []string{digest("not-the-token")}},
		{"the raw token, not its digest", []string{testToken}},
		{"empty", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := ctxWithToken("")
			defer cancel()
			for _, v := range tc.values {
				ctx = metadata.AppendToOutgoingContext(ctx, TokenHeader, v)
			}
			_, err := c.Judge(ctx, validRequest())
			wantCode(t, err, codes.Unauthenticated)
			_, err = c.Ping(ctx, &judgev1.PingRequest{})
			wantCode(t, err, codes.Unauthenticated)
		})
	}
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times for unauthenticated requests", n)
	}

	ctx, cancel := ctxWithToken(testToken)
	defer cancel()
	if _, err := c.Judge(ctx, validRequest()); err != nil {
		t.Fatalf("Judge with the right token: %v", err)
	}
}

// With no TOKEN configured the token check is off (it is additive to mTLS).
func TestAuthNoTokenConfigured(t *testing.T) {
	r := startRig(t, rigOptions{cfg: func(c *config.Config) { c.Token = "" }})
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}})
	ctx, cancel := ctxWithToken("")
	defer cancel()
	if _, err := r.client(t, testutil.WorkerCN).Judge(ctx, validRequest()); err != nil {
		t.Fatalf("Judge without a configured token: %v", err)
	}
}

func TestAuthStrangerCN(t *testing.T) {
	r := startRig(t, rigOptions{})
	c := r.client(t, testutil.StrangerCN)
	ctx, cancel := ctxWithToken(testToken)
	defer cancel()

	_, err := c.Judge(ctx, validRequest())
	wantCode(t, err, codes.PermissionDenied)
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times for a stranger", n)
	}

	if _, err := c.Ping(ctx, &judgev1.PingRequest{}); err != nil {
		t.Fatalf("Ping as a CN outside the allow-list: %v", err)
	}
}

// The Ping exemption is for the CN only: the token is still required.
func TestAuthPingStillNeedsTheToken(t *testing.T) {
	r := startRig(t, rigOptions{})
	ctx, cancel := ctxWithToken("")
	defer cancel()
	_, err := r.client(t, testutil.StrangerCN).Ping(ctx, &judgev1.PingRequest{})
	wantCode(t, err, codes.Unauthenticated)
}

func TestAuthNoClientCertHandshakeFails(t *testing.T) {
	r := startRig(t, rigOptions{})
	ctx, cancel := ctxWithToken(testToken)
	defer cancel()

	_, err := r.client(t, "").Ping(ctx, &judgev1.PingRequest{})
	wantCode(t, err, codes.Unavailable)
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times", n)
	}
}

// A client certificate signed by some other CA is refused at the handshake too.
func TestAuthForeignCAHandshakeFails(t *testing.T) {
	r := startRig(t, rigOptions{})
	other := testutil.TLS(t)
	cfg := r.mat.ClientConfig(t, "") // trusts the rig's server
	cert, err := tls.LoadX509KeyPair(filepath.Join(other.Dir, testutil.WorkerCN+".crt"), filepath.Join(other.Dir, testutil.WorkerCN+".key"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Certificates = []tls.Certificate{cert}

	ctx, cancel := ctxWithToken(testToken)
	defer cancel()
	_, err = r.clientWith(t, credentials.NewTLS(cfg)).Ping(ctx, &judgev1.PingRequest{})
	wantCode(t, err, codes.Unavailable)
}

// The server refuses TLS below 1.2.
func TestTLSConfigMinVersion(t *testing.T) {
	r := startRig(t, rigOptions{})
	cfg := r.mat.ClientConfig(t, testutil.WorkerCN)
	cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS10, tls.VersionTLS11

	ctx, cancel := ctxWithToken(testToken)
	defer cancel()
	_, err := r.clientWith(t, credentials.NewTLS(cfg)).Ping(ctx, &judgev1.PingRequest{})
	wantCode(t, err, codes.Unavailable)
}

func TestTLSConfig(t *testing.T) {
	mat := testutil.TLS(t)
	cfg, err := TLSConfig(mat.Dir)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.ClientCAs == nil || len(cfg.Certificates) != 1 {
		t.Errorf("ClientCAs = %v, Certificates = %d", cfg.ClientCAs, len(cfg.Certificates))
	}
}

func TestTLSConfigFailsClosed(t *testing.T) {
	copyDir := func(t *testing.T, skip string) string {
		t.Helper()
		mat := testutil.TLS(t)
		dir := t.TempDir()
		for _, n := range []string{"ca.crt", "tls.crt", "tls.key"} {
			if n == skip {
				continue
			}
			data, err := os.ReadFile(filepath.Join(mat.Dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, n), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for _, missing := range []string{"ca.crt", "tls.crt", "tls.key"} {
		if _, err := TLSConfig(copyDir(t, missing)); err == nil {
			t.Errorf("TLSConfig accepted a dir without %s", missing)
		}
	}
	dir := copyDir(t, "")
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TLSConfig(dir); err == nil {
		t.Error("TLSConfig accepted a ca.crt with no certificate")
	}
	if _, err := TLSConfig(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("TLSConfig accepted a missing dir")
	}
}

// peerCtx is a context as the TLS transport builds it for a verified client.
func peerCtx(cn string, token ...string) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}},
	}})
	if len(token) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.MD{TokenHeader: token})
	}
	return ctx
}

func TestAuthInterceptor(t *testing.T) {
	judgeInfo := &grpc.UnaryServerInfo{FullMethod: judgev1.JudgeServer_Judge_FullMethodName}
	pingInfo := &grpc.UnaryServerInfo{FullMethod: judgev1.JudgeServer_Ping_FullMethodName}
	sum := digest("tok")

	plain := peer.NewContext(context.Background(), &peer.Peer{}) // no TLS at all
	noChain := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}})

	tests := []struct {
		name    string
		allowed []string
		token   string // the digest the server wants; "" = no token required
		ctx     context.Context
		info    *grpc.UnaryServerInfo
		want    codes.Code
	}{
		{"allowed CN, no token configured", []string{"w"}, "", peerCtx("w"), judgeInfo, codes.OK},
		{"allowed CN, right token", []string{"w"}, sum, peerCtx("w", sum), judgeInfo, codes.OK},
		{"allowed CN, wrong token", []string{"w"}, sum, peerCtx("w", digest("x")), judgeInfo, codes.Unauthenticated},
		{"allowed CN, no token", []string{"w"}, sum, peerCtx("w"), judgeInfo, codes.Unauthenticated},
		{"allowed CN, empty token", []string{"w"}, sum, peerCtx("w", ""), judgeInfo, codes.Unauthenticated},
		{"allowed CN, token with a prefix of the digest", []string{"w"}, sum, peerCtx("w", sum[:10]), judgeInfo, codes.Unauthenticated},
		{"allowed CN, token with extra bytes", []string{"w"}, sum, peerCtx("w", sum+"0"), judgeInfo, codes.Unauthenticated},
		{"second of two token values matches", []string{"w"}, sum, peerCtx("w", "junk", sum), judgeInfo, codes.OK},
		{"other CN", []string{"w"}, "", peerCtx("s"), judgeInfo, codes.PermissionDenied},
		{"CN match is exact", []string{"w"}, "", peerCtx("w2"), judgeInfo, codes.PermissionDenied},
		{"CN match is case sensitive", []string{"w"}, "", peerCtx("W"), judgeInfo, codes.PermissionDenied},
		{"empty allow-list denies everyone", nil, "", peerCtx("w"), judgeInfo, codes.PermissionDenied},
		{"empty allow-list does not match an empty CN", []string{}, "", peerCtx(""), judgeInfo, codes.PermissionDenied},
		{"a blank allow-list entry does not admit an empty CN", []string{"", "w"}, "", peerCtx(""), judgeInfo, codes.PermissionDenied},
		{"no peer at all", []string{"w"}, "", context.Background(), judgeInfo, codes.PermissionDenied},
		{"plaintext peer", []string{"w"}, "", plain, judgeInfo, codes.PermissionDenied},
		{"TLS without a verified chain", []string{"w"}, "", noChain, judgeInfo, codes.PermissionDenied},
		{"ping exempts the CN", []string{"w"}, "", peerCtx("s"), pingInfo, codes.OK},
		{"ping exempts the CN with an empty allow-list", nil, "", peerCtx("s"), pingInfo, codes.OK},
		{"ping still needs the token", []string{"w"}, sum, peerCtx("s"), pingInfo, codes.Unauthenticated},
		{"ping with the token", []string{"w"}, sum, peerCtx("s", sum), pingInfo, codes.OK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			quietLogs(t)
			called := false
			handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }
			resp, err := AuthInterceptor(tc.allowed, tc.token)(tc.ctx, nil, tc.info, handler)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tc.want)
			}
			if called != (tc.want == codes.OK) {
				t.Fatalf("handler called = %v with code %v", called, tc.want)
			}
			if tc.want == codes.OK && resp != "ok" {
				t.Fatalf("response = %v, want the handler's", resp)
			}
		})
	}
}

// JudgeStream runs behind the stream interceptor: the same token and CN checks
// as Judge, and the engine is never reached by a refused call.
func TestAuthCoversJudgeStream(t *testing.T) {
	r := startRig(t, rigOptions{})
	r.fake.fn = answers(judge.Outcome{Cases: []judge.CaseResult{{Name: "1"}}})

	noToken, cancel := ctxWithToken("")
	defer cancel()
	_, err := collect(r.client(t, testutil.WorkerCN).JudgeStream(noToken, validRequest()))
	wantCode(t, err, codes.Unauthenticated)

	wrong := metadata.AppendToOutgoingContext(noToken, TokenHeader, digest("not-the-token"))
	_, err = collect(r.client(t, testutil.WorkerCN).JudgeStream(wrong, validRequest()))
	wantCode(t, err, codes.Unauthenticated)

	ctx, cancel2 := ctxWithToken(testToken)
	defer cancel2()
	_, err = collect(r.client(t, testutil.StrangerCN).JudgeStream(ctx, validRequest()))
	wantCode(t, err, codes.PermissionDenied)
	if n := r.fake.calls(); n != 0 {
		t.Fatalf("engine called %d times for refused streams", n)
	}

	if _, err := r.judgeStream(t, validRequest()); err != nil {
		t.Fatalf("JudgeStream as the worker with the right token: %v", err)
	}
}

// The stream interceptor is the unary one's checks on a ServerStream.
func TestAuthStreamInterceptor(t *testing.T) {
	sum := digest("tok")
	info := &grpc.StreamServerInfo{FullMethod: judgev1.JudgeServer_JudgeStream_FullMethodName}
	tests := []struct {
		name    string
		allowed []string
		token   string
		ctx     context.Context
		want    codes.Code
	}{
		{"allowed CN, right token", []string{"w"}, sum, peerCtx("w", sum), codes.OK},
		{"allowed CN, no token", []string{"w"}, sum, peerCtx("w"), codes.Unauthenticated},
		{"other CN", []string{"w"}, "", peerCtx("s"), codes.PermissionDenied},
		{"no peer at all", []string{"w"}, "", context.Background(), codes.PermissionDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			quietLogs(t)
			called := false
			handler := func(any, grpc.ServerStream) error { called = true; return nil }
			err := AuthStreamInterceptor(tc.allowed, tc.token)(nil, ctxStream{ctx: tc.ctx}, info, handler)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tc.want)
			}
			if called != (tc.want == codes.OK) {
				t.Fatalf("handler called = %v with code %v", called, tc.want)
			}
		})
	}
}

// ctxStream is a ServerStream that only knows its context.
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }
