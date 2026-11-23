package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownFiles(dir string) tlsFiles {
	return tlsFiles{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem"), Own: true}
}

func TestHydraMakesACAThatOnlyVouchesForThisMachine(t *testing.T) {
	dir := t.TempDir()
	f := ownFiles(dir)
	if err := ensureTLS(f); err != nil {
		t.Fatal(err)
	}
	ca, err := readCert(f.CA)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.PermittedDNSDomainsCritical || len(ca.PermittedDNSDomains) != 1 || ca.PermittedDNSDomains[0] != "localhost" || len(ca.PermittedIPRanges) != 2 {
		t.Errorf("CA constraints: dns %v (critical %v), ips %v", ca.PermittedDNSDomains, ca.PermittedDNSDomainsCritical, ca.PermittedIPRanges)
	}
	if info, _ := os.Stat(filepath.Join(dir, "ca-key.pem")); info == nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the CA's key should be private to the user: %v", info)
	}
	pair, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, name := range []string{"localhost", "web.localhost"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Error(err)
	}
	if left := time.Until(leaf.NotAfter); left > 825*24*time.Hour {
		t.Errorf("the certificate lives %s; Apple accepts at most 825 days", left)
	}

	// The CA can't be made to vouch for anything else: a certificate for a
	// real site, signed with its key, is refused because of the constraints.
	_, caKey, err := loadCA(f.CA, filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	rogueKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"example.com"},
	}, ca, &rogueKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	rogue, _ := x509.ParseCertificate(der)
	if _, err := rogue.Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com"}); err == nil {
		t.Error("a certificate for example.com signed by hydra's CA verified")
	} else if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("refused, but not for the constraints: %v", err)
	}

	// A second call leaves everything as it is.
	before, _ := os.ReadFile(f.Cert)
	if err := ensureTLS(f); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(f.Cert); string(after) != string(before) {
		t.Error("ensureTLS made a new certificate though the old one was good")
	}

	// A certificate from another CA is replaced by one this CA signed.
	otherCA := ownFiles(t.TempDir())
	if err := ensureTLS(otherCA); err != nil {
		t.Fatal(err)
	}
	os.Rename(otherCA.Cert, f.Cert)
	os.Rename(otherCA.Key, f.Key)
	if certGood(f, ca) {
		t.Fatal("another CA's certificate passed for hydra's")
	}
	if err := ensureTLS(f); err != nil {
		t.Fatal(err)
	}
	if !certGood(f, ca) {
		t.Error("the certificate wasn't renewed under hydra's CA")
	}
}

func TestAFreshCAIsNotTrustedUntilAsked(t *testing.T) {
	f := ownFiles(t.TempDir())
	if err := ensureTLS(f); err != nil {
		t.Fatal(err)
	}
	// Nobody ran `hydra trust` for this one.
	if trusted(f) {
		t.Error("a CA made a moment ago is trusted by this machine")
	}
}

func TestFrontCertIsServable(t *testing.T) {
	f := ownFiles(t.TempDir())
	cert, err := frontCert(f)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	ca, _ := readCert(f.CA)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(), &tls.Config{RootCAs: pool})
	if err != nil {
		t.Fatalf("a client that trusts the CA couldn't connect: %v", err)
	}
	c.Close()
	// The same parsed certificate is handed out until the file changes.
	again, _ := frontCert(f)
	if again != cert {
		t.Error("frontCert parsed the files again")
	}
}
