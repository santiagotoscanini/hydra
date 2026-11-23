package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// TLS fronts need a certificate that browsers and processes on this machine
// accept. hydra keeps one local CA per user (so trusting it once covers every
// repository) and a certificate signed by it. A stack can bring its own
// instead, with x-hydra.tls.
//
// The CA can only vouch for this machine: its name constraints allow
// "localhost", names under it, and loopback addresses, and nothing else. Its
// key leaking doesn't let anyone pose as a real site to this browser, which
// is more than can be said of an unconstrained local CA.

// tlsFiles are the files TLS fronts use. Own is true for hydra's own CA and
// certificate, which it makes and renews; false for a stack's.
type tlsFiles struct {
	Cert, Key, CA string
	Own           bool
}

// tlsDir is where hydra keeps its CA and certificate.
func tlsDir() (string, error) {
	if d := os.Getenv("HYDRA_TLS_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "hydra"), nil
}

// tlsFilesFor returns the stack's own certificate files if it names any,
// hydra's otherwise. It creates nothing.
func tlsFilesFor(t *target) (tlsFiles, error) {
	own := t.stack.Hydra.TLS
	if own.Cert != "" || own.Key != "" {
		if own.Cert == "" || own.Key == "" {
			return tlsFiles{}, fmt.Errorf("x-hydra.tls needs both cert and key")
		}
		abs := func(p string) string {
			if p == "" || filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(filepath.Dir(t.file), p)
		}
		return tlsFiles{Cert: abs(own.Cert), Key: abs(own.Key), CA: abs(own.CA)}, nil
	}
	dir, err := tlsDir()
	if err != nil {
		return tlsFiles{}, err
	}
	return tlsFiles{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem"), Own: true}, nil
}

const (
	caLife      = 10 * 365 * 24 * time.Hour
	certLife    = 800 * 24 * time.Hour // Apple accepts at most 825 days
	renewBefore = 30 * 24 * time.Hour
)

// ensureTLS makes hydra's CA if there is none, and a certificate when it is
// missing, about to expire, or signed by another CA. A stack's own files are
// only checked for being there.
func ensureTLS(f tlsFiles) error {
	if !f.Own {
		for _, p := range []string{f.Cert, f.Key} {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("x-hydra.tls: %w", err)
			}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.CA), 0o700); err != nil {
		return err
	}
	caKeyPath := filepath.Join(filepath.Dir(f.CA), "ca-key.pem")
	ca, caKey, err := loadCA(f.CA, caKeyPath)
	if err != nil {
		if ca, caKey, err = makeCA(f.CA, caKeyPath); err != nil {
			return err
		}
	}
	if certGood(f, ca) {
		return nil
	}
	return makeCert(f, ca, caKey)
}

func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := readCert(certPath)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, nil, fmt.Errorf("%s: not PEM", keyPath)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if time.Until(cert.NotAfter) < renewBefore {
		return nil, nil, errors.New("the CA is about to expire")
	}
	return cert, key, nil
}

func readCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func makeCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{"hydra"}, CommonName: "hydra local CA (" + host + ")"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caLife),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// Only this machine's own names and addresses.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
		PermittedIPRanges: []*net.IPNet{
			{IP: net.IPv4(127, 0, 0, 0).To4(), Mask: net.CIDRMask(8, 32)},
			{IP: net.IPv6loopback, Mask: net.CIDRMask(128, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, nil, err
	}
	if err := writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// certGood reports whether the certificate on disk is signed by ca, matches
// its key, and has a month or more to live.
func certGood(f tlsFiles, ca *x509.Certificate) bool {
	if _, err := tls.LoadX509KeyPair(f.Cert, f.Key); err != nil {
		return false
	}
	cert, err := readCert(f.Cert)
	if err != nil || time.Until(cert.NotAfter) < renewBefore {
		return false
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"})
	return err == nil
}

func makeCert(f tlsFiles, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"hydra"}, CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(certLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "*.localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeAtomic(f.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeAtomic(f.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// frontCert loads the certificate TLS fronts present, making hydra's own if
// needed. The router asks twice a second, so the parsed certificate is kept
// until its file changes.
var certCache struct {
	sync.Mutex
	path  string
	mtime time.Time
	cert  *tls.Certificate
}

func frontCert(f tlsFiles) (*tls.Certificate, error) {
	certCache.Lock()
	defer certCache.Unlock()
	info, err := os.Stat(f.Cert)
	if err == nil && certCache.cert != nil && certCache.path == f.Cert && info.ModTime().Equal(certCache.mtime) {
		if leaf := certCache.cert.Leaf; leaf != nil && time.Until(leaf.NotAfter) > renewBefore {
			return certCache.cert, nil
		}
	}
	if err := ensureTLS(f); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	if info, err = os.Stat(f.Cert); err != nil {
		return nil, err
	}
	certCache.path, certCache.mtime, certCache.cert = f.Cert, info.ModTime(), &cert
	return &cert, nil
}

// trusted reports whether this machine accepts the certificate: on macOS
// that asks the system, which knows what the keychains trust.
func trusted(f tlsFiles) bool {
	cert, err := readCert(f.Cert)
	if err != nil {
		return false
	}
	opts := x509.VerifyOptions{DNSName: "localhost"}
	if !f.Own && f.CA != "" {
		// A stack's own chain may need its CA as a link to a trusted root.
		if ca, err := readCert(f.CA); err == nil {
			opts.Intermediates = x509.NewCertPool()
			opts.Intermediates.AddCert(ca)
		}
	}
	_, err = cert.Verify(opts)
	return err == nil
}

// trustCmd makes this machine trust hydra's CA, or stops it.
//
//	hydra trust [--remove] [--print]
func trustCmd(args []string) error {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	remove := fs.Bool("remove", false, "")
	print := fs.Bool("print", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := tlsDir()
	if err != nil {
		return err
	}
	f := tlsFiles{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem"), Own: true}
	if *print {
		fmt.Println(f.CA)
		return nil
	}
	if *remove {
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("remove %s from your system's trusted authorities by hand; hydra only knows how on macOS", f.CA)
		}
		if out, err := exec.Command("security", "remove-trusted-cert", f.CA).CombinedOutput(); err != nil {
			return fmt.Errorf("security remove-trusted-cert: %s", strings.TrimSpace(string(out)))
		}
		fmt.Println("hydra: this Mac no longer trusts", f.CA)
		return nil
	}
	if err := ensureTLS(f); err != nil {
		return err
	}
	if trusted(f) {
		fmt.Println("hydra: this machine already trusts", f.CA)
		return nil
	}
	if runtime.GOOS != "darwin" {
		fmt.Printf("hydra's CA is %s.\nAdd it to your system's (and your browser's) trusted authorities; hydra only knows how on macOS.\n", f.CA)
		return nil
	}
	keychain, err := exec.Command("security", "default-keychain", "-d", "user").Output()
	if err != nil {
		return fmt.Errorf("finding your keychain: %w", err)
	}
	kc := strings.Trim(strings.TrimSpace(string(keychain)), `"`)
	fmt.Printf("hydra: adding %s to %s, trusted for TLS only.\n", f.CA, kc)
	fmt.Println("hydra: it can only vouch for localhost and loopback addresses. macOS will ask you to confirm.")
	cmd := exec.Command("security", "add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", kc, f.CA)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add-trusted-cert: %w", err)
	}
	if !trusted(f) {
		return fmt.Errorf("macOS took the certificate but still doesn't accept it; open Keychain Access, find \"hydra local CA\" and set it to Always Trust")
	}
	fmt.Println("hydra: done. Browsers that use the system's trust (Safari, Chrome) accept hydra's TLS fronts now; Firefox keeps its own list.")
	return nil
}
