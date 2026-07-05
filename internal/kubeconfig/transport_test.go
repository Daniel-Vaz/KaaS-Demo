package kubeconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"
)

func TestNewEndpointToken(t *testing.T) {
	kc := []byte(`apiVersion: v1
clusters:
- name: c
  cluster:
    server: https://10.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
users:
- name: u
  user:
    token: sa-token-xyz
`)
	ep, err := NewEndpoint(kc, "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	if ep.Server != "https://10.0.0.1:6443" {
		t.Fatalf("Server = %q", ep.Server)
	}
	if ep.Token != "sa-token-xyz" {
		t.Fatalf("Token = %q", ep.Token)
	}
	tr, ok := ep.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("expected an http.Transport with a proxy set")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("insecure-skip-tls-verify should carry through")
	}
}

func TestNewEndpointClientCert(t *testing.T) {
	certPEM, keyPEM := selfSigned(t)
	b64 := base64.StdEncoding.EncodeToString
	kc := []byte(`apiVersion: v1
clusters:
- name: c
  cluster:
    server: https://10.0.0.1:6443
    certificate-authority-data: ` + b64(certPEM) + `
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
users:
- name: u
  user:
    client-certificate-data: ` + b64(certPEM) + `
    client-key-data: ` + b64(keyPEM) + `
`)
	ep, err := NewEndpoint(kc, "")
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	if ep.Token != "" {
		t.Fatalf("cert-auth kubeconfig should carry no token, got %q", ep.Token)
	}
	tr := ep.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatalf("no proxy expected when proxyURL empty and none embedded")
	}
	if n := len(tr.TLSClientConfig.Certificates); n != 1 {
		t.Fatalf("client certificates = %d, want 1", n)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Fatalf("CA pool should be set from certificate-authority-data")
	}
}

// selfSigned returns a throwaway cert+key PEM pair for the client-cert path.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// sanity: the pair must load
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM
}
