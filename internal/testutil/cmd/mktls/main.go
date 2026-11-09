// Command mktls writes a throw-away PKI for the judge-server smoke test:
//
//	go run ./internal/testutil/cmd/mktls <dir> [extra-server-SAN...]
//
// <dir> receives ca.crt, tls.crt and tls.key (the server's, the /etc/oj/tls
// layout) and judge-worker.{crt,key} and stranger.{crt,key} (clients signed by
// the same CA). The server certificate is valid for judge-server, localhost,
// 127.0.0.1, ::1, this host's name and every extra SAN given (a DNS name or an
// IP), e.g. the compose service's hostname the healthcheck resolves. Test
// material only: the keys are not protected beyond file mode 0600.
package main

import (
	"fmt"
	"os"

	"github.com/My-Online-Judge/judge-server/internal/testutil"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mktls <dir> [extra-server-SAN...]")
		os.Exit(2)
	}
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mktls: %v\n", err)
		os.Exit(1)
	}
	if err := testutil.WriteTLS(dir, os.Args[2:]...); err != nil {
		fmt.Fprintf(os.Stderr, "mktls: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote ca.crt tls.crt tls.key judge-worker.{crt,key} stranger.{crt,key} to %s\n", dir)
}
