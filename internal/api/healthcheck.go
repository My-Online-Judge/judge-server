package api

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/My-Online-Judge/judge-server/internal/config"
	"github.com/My-Online-Judge/judge-server/internal/pb/judgev1"
)

// healthcheckTimeout bounds the whole probe: dial, handshake and Ping.
const healthcheckTimeout = 3 * time.Second

// Healthcheck pings the server on this host over mutual TLS and returns nil only
// when it answers. It dials 127.0.0.1 at the port of cfg.Listen, verifying the
// server certificate for this host's name (the one the certificate is issued for)
// and presenting that same certificate as its client certificate: the PKI issues
// it with both extended key usages, and Ping admits any CA-signed client. The
// token, when configured, goes in the metadata as for any other call.
func Healthcheck(cfg config.Config) error {
	port, err := healthcheckPort(cfg.Listen)
	if err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("host name: %w", err)
	}
	creds, err := clientCreds(cfg.TLSDir, host)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(net.JoinHostPort("127.0.0.1", port), grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if sum := tokenDigest(cfg.Token); sum != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, TokenHeader, sum)
	}
	if _, err := judgev1.NewJudgeServerClient(conn).Ping(ctx, &judgev1.PingRequest{}); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// healthcheckPort extracts the TCP port from a listen address such as ":8080",
// "0.0.0.0:8080" or "[::]:8080".
func healthcheckPort(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("LISTEN %q: %w", listen, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("LISTEN %q: %q is not a port from 1 to 65535", listen, port)
	}
	return port, nil
}
