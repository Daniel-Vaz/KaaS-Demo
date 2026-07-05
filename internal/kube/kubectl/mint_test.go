package kubectl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"gopkg.in/yaml.v3"
)

// mintStub plays the cluster's CSR API back at the mint: it captures the submitted CSR, signs it
// with a throwaway CA on the `get` call, and records that approve happened - enough to prove the
// mint carries the right identity, approves, and assembles a usable kubeconfig, all without a real
// API server. It satisfies both Execer (Run/Stream) and the optional inputExecer (RunInput).
type mintStub struct {
	t        *testing.T
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	notAfter time.Time

	gotCSR   *x509.CertificateRequest
	approved bool
	deleted  bool
	issued   []byte // PEM of the signed cert, populated on create
}

func (s *mintStub) Run(ctx context.Context, kc []byte, id string, args []string) (Result, error) {
	return s.RunInput(ctx, kc, id, nil, args)
}

func (s *mintStub) Stream(context.Context, []byte, string, []string, kube.LogSink) error { return nil }

func (s *mintStub) RunInput(_ context.Context, _ []byte, _ string, stdin []byte, args []string) (Result, error) {
	switch {
	case len(args) >= 1 && args[0] == "create":
		s.sign(stdin)
		return Result{}, nil
	case len(args) >= 2 && args[0] == "certificate" && args[1] == "approve":
		s.approved = true
		return Result{}, nil
	case len(args) >= 2 && args[0] == "get" && args[1] == "csr":
		if !s.approved {
			return Result{}, nil // not issued until approved
		}
		return Result{Stdout: []byte(base64.StdEncoding.EncodeToString(s.issued))}, nil
	case len(args) >= 1 && args[0] == "delete":
		s.deleted = true
		return Result{}, nil
	}
	s.t.Fatalf("mintStub: unexpected kubectl args %v", args)
	return Result{}, nil
}

// sign parses the submitted CSR object off stdin and issues a cert for it, mimicking the signer.
func (s *mintStub) sign(stdin []byte) {
	var obj struct {
		Spec struct {
			Request           string `yaml:"request"`
			SignerName        string `yaml:"signerName"`
			ExpirationSeconds int64  `yaml:"expirationSeconds"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(stdin, &obj); err != nil {
		s.t.Fatalf("mintStub: parse CSR object: %v", err)
	}
	if obj.Spec.SignerName != kubeAPIServerClientSigner {
		s.t.Fatalf("mintStub: signerName = %q, want %q", obj.Spec.SignerName, kubeAPIServerClientSigner)
	}
	csrPEM, err := base64.StdEncoding.DecodeString(obj.Spec.Request)
	if err != nil {
		s.t.Fatalf("mintStub: decode request: %v", err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		s.t.Fatal("mintStub: request is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		s.t.Fatalf("mintStub: parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		s.t.Fatalf("mintStub: CSR self-signature invalid: %v", err)
	}
	s.gotCSR = csr

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      csr.Subject,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     s.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, csr.PublicKey, s.caKey)
	if err != nil {
		s.t.Fatalf("mintStub: sign cert: %v", err)
	}
	s.issued = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestMintUserKubeconfig(t *testing.T) {
	// A throwaway CA + an admin kubeconfig that points at a VIP and trusts it.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	const server = "https://10.0.0.9:6443"
	adminKC := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: " + server +
		"\n    certificate-authority-data: " + base64.StdEncoding.EncodeToString(caPEM) + "\n")

	notAfter := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	stub := &mintStub{t: t, ca: ca, caKey: caKey, notAfter: notAfter}
	cl := &domain.Cluster{ID: "cl1", Name: "prod"}

	out, gotNotAfter, err := New(stub).MintUserKubeconfig(context.Background(), cl, adminKC, "dvaz", domain.GroupRoleWrite, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("MintUserKubeconfig: %v", err)
	}

	// The CSR carried the caller's identity and the write role's group.
	if s := stub.gotCSR.Subject; s.CommonName != "dvaz" || len(s.Organization) != 1 || s.Organization[0] != domain.KubeGroupWriters {
		t.Fatalf("CSR subject = CN=%q O=%v, want CN=dvaz O=[%s]", s.CommonName, s.Organization, domain.KubeGroupWriters)
	}
	if !stub.approved {
		t.Fatal("CSR was never approved")
	}
	if !stub.deleted {
		t.Fatal("transient CSR was not cleaned up")
	}
	if !gotNotAfter.Equal(notAfter) {
		t.Fatalf("notAfter = %s, want %s (the issued cert's, not the requested TTL)", gotNotAfter, notAfter)
	}

	// The assembled kubeconfig targets the cluster's own endpoint and embeds a usable client cert+key.
	gotServer, gotCA, err := kubeconfig.ClusterEndpoint(out)
	if err != nil {
		t.Fatalf("assembled kubeconfig unparsable: %v", err)
	}
	if gotServer != server {
		t.Fatalf("server = %q, want %q (copied from admin config)", gotServer, server)
	}
	if gotCA != base64.StdEncoding.EncodeToString(caPEM) {
		t.Fatal("CA data was not copied from the admin config")
	}
	if !strings.Contains(string(out), "client-certificate-data:") || !strings.Contains(string(out), "client-key-data:") {
		t.Fatal("assembled kubeconfig is missing the client cert/key")
	}
}

// TestMintUserKubeconfigReaderGroup: the read role maps to kaas:readers.
func TestMintUserKubeconfigReaderGroup(t *testing.T) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	adminKC := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://1.2.3.4:6443\n    certificate-authority-data: " + base64.StdEncoding.EncodeToString(caPEM) + "\n")

	stub := &mintStub{t: t, ca: ca, caKey: caKey, notAfter: time.Now().Add(time.Hour)}
	if _, _, err := New(stub).MintUserKubeconfig(context.Background(), &domain.Cluster{ID: "c", Name: "c"}, adminKC, "bob", domain.GroupRoleRead, time.Hour); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if org := stub.gotCSR.Subject.Organization; len(org) != 1 || org[0] != domain.KubeGroupReaders {
		t.Fatalf("reader CSR O=%v, want [%s]", org, domain.KubeGroupReaders)
	}
}
