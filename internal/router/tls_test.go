package router

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// selfSigned is a certificate for 127.0.0.1 and the pool that trusts it.
func selfSigned(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func TestTLSFrontCarriesHTTP1AndHTTP2(t *testing.T) {
	cert, pool := selfSigned(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s over %s, tls on the service's side: %v", r.URL.Path, r.Proto, r.TLS != nil)
	})
	// A plain HTTP/1.1 service, and one that speaks HTTP/2 in the clear as a
	// gRPC server does.
	plain := httptest.NewServer(handler)
	defer plain.Close()
	h2c := httptest.NewUnstartedServer(handler)
	h2c.Config.Protocols = new(http.Protocols)
	h2c.Config.Protocols.SetUnencryptedHTTP2(true)
	h2c.Start()
	defer h2c.Close()

	front1, front2 := freePort(t), freePort(t)
	st := &State{Host: "127.0.0.1", Cert: cert, Fronts: map[string]Front{
		"web":  {Listen: front1, Target: portOf(t, plain), ALPN: "http/1.1"},
		"grpc": {Listen: front2, Target: portOf(t, h2c), ALPN: "h2"},
	}}
	r := &Router{Load: func() (*State, error) { return st, nil }}
	defer r.Close()
	r.Reconcile()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}
	get := func(port int) string {
		t.Helper()
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/hello", port))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	// The client speaks TLS to the front; the service never sees it.
	if got := get(front1); got != "/hello over HTTP/1.1, tls on the service's side: false" {
		t.Errorf("http/1.1 front: %q", got)
	}
	if got := get(front2); got != "/hello over HTTP/2.0, tls on the service's side: false" {
		t.Errorf("h2 front: %q", got)
	}

	// A client that doesn't trust the certificate is turned away.
	untrusting := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}}
	if _, err := untrusting.Get(fmt.Sprintf("https://127.0.0.1:%d/", front1)); err == nil {
		t.Error("a client without the CA got through")
	}

	// A front that is taken out of the stack file closes.
	st = &State{Host: "127.0.0.1", Cert: cert, Fronts: map[string]Front{}}
	r.Reconcile()
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", front1), time.Second); err == nil {
		c.Close()
		t.Error("the front still listens after it was removed")
	}
}

func TestTLSFrontOfAServiceThatIsDown(t *testing.T) {
	cert, pool := selfSigned(t)
	front := freePort(t)
	st := &State{Host: "127.0.0.1", Cert: cert, Fronts: map[string]Front{"web": {Listen: front, Target: freePort(t), ALPN: "http/1.1"}}}
	r := &Router{Load: func() (*State, error) { return st, nil }}
	defer r.Close()
	r.Reconcile()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	// Nothing behind it: the connection closes, it doesn't hang.
	if _, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/", front)); err == nil {
		t.Error("want an error from a front with nothing behind it")
	}
}
