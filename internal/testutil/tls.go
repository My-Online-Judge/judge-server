package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Common names of the client certificates WriteTLS issues: the first is in the
// server's default allow-list, the second is signed by the same CA but is not.
const (
	WorkerCN   = "judge-worker"
	StrangerCN = "stranger"
	// ServerName is the first DNS name in the server certificate's SANs.
	ServerName = "judge-server"
)

// TLSMaterial is a throw-away PKI on disk: a CA, a server certificate and two
// client certificates, all signed by that CA.
type TLSMaterial struct {
	// Dir is laid out like /etc/oj/tls: ca.crt, tls.crt and tls.key (the server's),
	// plus <cn>.crt and <cn>.key for each client common name.
	Dir string
}

// TLS writes a fresh PKI into a temp dir with WriteTLS.
func TLS(t testing.TB) *TLSMaterial {
	t.Helper()
	dir := t.TempDir()
	if err := WriteTLS(dir); err != nil {
		t.Fatalf("testutil: write TLS material: %v", err)
	}
	return &TLSMaterial{Dir: dir}
}

// ClientConfig returns the TLS config of a client that presents the certificate
// issued for cn (WorkerCN or StrangerCN) and trusts the CA. An empty cn presents
// no certificate.
func (m *TLSMaterial) ClientConfig(t testing.TB, cn string) *tls.Config {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(m.Dir, "ca.crt"))
	if err != nil {
		t.Fatalf("testutil: read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("testutil: ca.crt holds no certificate")
	}
	cfg := &tls.Config{RootCAs: pool, ServerName: ServerName, MinVersion: tls.VersionTLS12}
	if cn != "" {
		cert, err := tls.LoadX509KeyPair(filepath.Join(m.Dir, cn+".crt"), filepath.Join(m.Dir, cn+".key"))
		if err != nil {
			t.Fatalf("testutil: load client pair %s: %v", cn, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg
}

// WriteTLS writes the PKI of TLSMaterial into dir (which must exist). The server
// certificate (CN judge-server) is valid for the names judge-server and localhost,
// 127.0.0.1 and ::1, this host's name, and extraSANs (a DNS name, or an IP in
// text form). Every leaf carries both extended key usages, as the production PKI
// issues them, because the server dials itself with its own certificate in the
// healthcheck.
func WriteTLS(dir string, extraSANs ...string) error {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "judge-server test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER, 0o644); err != nil {
		return err
	}

	server := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: ServerName},
		DNSNames:     []string{ServerName, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		server.DNSNames = append(server.DNSNames, host)
	}
	for _, san := range extraSANs {
		if ip := net.ParseIP(san); ip != nil {
			server.IPAddresses = append(server.IPAddresses, ip)
		} else {
			server.DNSNames = append(server.DNSNames, san)
		}
	}
	if err := issue(dir, "tls", server, ca, caKey, now); err != nil {
		return err
	}
	for _, cn := range []string{WorkerCN, StrangerCN} {
		client := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: cn}}
		if err := issue(dir, cn, client, ca, caKey, now); err != nil {
			return err
		}
	}
	return nil
}

// issue signs tmpl with the CA and writes <name>.crt and <name>.key into dir.
func issue(dir, name string, tmpl, ca *x509.Certificate, caKey *ecdsa.PrivateKey, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl.NotBefore = now.Add(-time.Hour)
	tmpl.NotAfter = now.Add(24 * time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, name+".crt"), "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, name+".key"), "PRIVATE KEY", keyDER, 0o600)
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	data := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// serial returns a random 126-bit certificate serial number.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		panic(err) // crypto/rand does not fail on a working system
	}
	return n
}
