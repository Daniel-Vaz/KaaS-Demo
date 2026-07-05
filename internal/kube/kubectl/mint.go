package kubectl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
)

// kubeAPIServerClientSigner is the built-in signer whose certs the kube-apiserver honours for client
// authentication; kube-controller-manager auto-issues approved CSRs for it using the cluster CA. It
// refuses to sign a CSR whose Organization is system:masters - which is exactly why cluster-admin
// here comes from an RBAC binding on the kaas:writers group, not from that group.
const kubeAPIServerClientSigner = "kubernetes.io/kube-apiserver-client"

// minExpirationSeconds is the floor the signer enforces on a requested cert lifetime.
const minExpirationSeconds = 600

// inputExecer is the optional stdin-carrying Execer the mint needs to `kubectl create -f -` a CSR.
// Both real Execers (LocalExecer, proxy.Execer) satisfy it; a plain Execer (e.g. a read-only test
// stub) does not, and the mint reports that rather than silently failing.
type inputExecer interface {
	RunInput(ctx context.Context, kubeconfig []byte, clusterID string, stdin []byte, args []string) (Result, error)
}

// MintUserKubeconfig issues a per-user client-certificate kubeconfig via the CertificateSigningRequest
// API (see the kube.Client interface doc). The private key is generated here and never leaves the
// process: only the CSR (public) and the signed cert cross to the exec agent, and only the admin
// kubeconfig authenticates the create/approve. The result copies the cluster's own API-server URL
// (the HA VIP when there is one) and CA out of that admin config.
func (c *Client) MintUserKubeconfig(ctx context.Context, cl *domain.Cluster, adminKC []byte, username string, role domain.GroupRole, ttl time.Duration) ([]byte, time.Time, error) {
	in, ok := c.ex.(inputExecer)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("kube exec backend does not support stdin - cannot mint a user kubeconfig")
	}
	if strings.TrimSpace(username) == "" {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: empty username")
	}

	server, caData, err := kubeconfig.ClusterEndpoint(adminKC)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: read cluster endpoint: %w", err)
	}

	// 1. Generate a key + CSR carrying the directory identity: CN=username, O=the role's kube group.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: generate key: %w", err)
	}
	group := domain.KubeGroupForRole(role)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: username, Organization: []string{group}},
	}, key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// 2. Submit the CSR object. A random name keeps concurrent mints (and replicas) from colliding on
	// the object name, which must be DNS-1123 regardless of what the username looks like.
	name, err := csrName()
	if err != nil {
		return nil, time.Time{}, err
	}
	expSecs := int64(ttl.Seconds())
	if expSecs < minExpirationSeconds {
		expSecs = minExpirationSeconds
	}
	manifest := fmt.Sprintf(`apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: %s
spec:
  request: %s
  signerName: %s
  expirationSeconds: %d
  usages:
  - client auth
`, name, base64.StdEncoding.EncodeToString(csrPEM), kubeAPIServerClientSigner, expSecs)

	if err := runInput(ctx, in, adminKC, cl.ID, []byte(manifest), "create", "-f", "-"); err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: submit CSR: %w", err)
	}
	// The CSR object is transient - always clean it up, success or failure past this point.
	defer func() {
		_, _ = in.RunInput(context.WithoutCancel(ctx), adminKC, cl.ID, nil, []string{"delete", "csr", name, "--ignore-not-found"})
	}()

	// 3. Approve it, then read back the issued certificate (the signer populates it moments later).
	if err := runInput(ctx, in, adminKC, cl.ID, nil, "certificate", "approve", name); err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: approve CSR: %w", err)
	}
	certPEM, err := awaitCertificate(ctx, in, adminKC, cl.ID, name)
	if err != nil {
		return nil, time.Time{}, err
	}

	// 4. Parse the real NotAfter (the signer may cap the requested TTL) and assemble the kubeconfig.
	notAfter, err := certNotAfter(certPEM)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mint kubeconfig: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	out, err := kubeconfig.BuildClientCert(cl.Name, username, server, caData, certPEM, keyPEM)
	if err != nil {
		return nil, time.Time{}, err
	}
	return out, notAfter, nil
}

// runInput runs one kubectl command through the stdin-capable executor, turning a non-zero exit into
// a Go error (mirroring Client.run, which the query methods use).
func runInput(ctx context.Context, in inputExecer, kc []byte, id string, stdin []byte, args ...string) error {
	res, err := in.RunInput(ctx, kc, id, stdin, args)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("kubectl exited %d", res.Code)
		}
		return fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

// awaitCertificate polls the CSR's issued certificate, which the signer controller writes a moment
// after approval. A few short retries cover that gap without making the download feel stuck.
func awaitCertificate(ctx context.Context, in inputExecer, kc []byte, id, name string) ([]byte, error) {
	for attempt := 0; attempt < 12; attempt++ {
		res, err := in.RunInput(ctx, kc, id, nil, []string{"get", "csr", name, "-o", "jsonpath={.status.certificate}"})
		if err != nil {
			return nil, fmt.Errorf("mint kubeconfig: read issued cert: %w", err)
		}
		if res.Code == 0 && len(strings.TrimSpace(string(res.Stdout))) > 0 {
			certPEM, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(res.Stdout)))
			if derr != nil {
				return nil, fmt.Errorf("mint kubeconfig: decode issued cert: %w", derr)
			}
			return certPEM, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("mint kubeconfig: certificate for %s was not issued in time", name)
}

// certNotAfter returns the leaf certificate's expiry from its PEM.
func certNotAfter(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return time.Time{}, fmt.Errorf("issued certificate is not valid PEM")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse issued certificate: %w", err)
	}
	return crt.NotAfter, nil
}

// csrName mints a unique, DNS-1123 name for the transient CSR object.
func csrName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint kubeconfig: random name: %w", err)
	}
	return "kaas-user-" + hex.EncodeToString(b[:]), nil
}
