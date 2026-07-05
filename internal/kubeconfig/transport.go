package kubeconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"gopkg.in/yaml.v3"
)

// Endpoint is a ready-to-use HTTP target derived from a kubeconfig's current context: the API
// server's base URL, a RoundTripper carrying its TLS trust and client-certificate auth (plus a SOCKS
// proxy when the KVM host is remote), and a bearer Token to set per request when the kubeconfig
// authenticates by token (a ServiceAccount, e.g. the read-only viewer) rather than a client cert.
//
// It exists so the exec agent can build its own reverse proxy to the API server without client-go
// (not a dependency here) - the tunnel seam (internal/tunnel) needs a full streaming HTTP transport,
// not the `kubectl get --raw` the other query seams shell out to.
type Endpoint struct {
	Server    string
	Transport http.RoundTripper
	Token     string
}

// minimal kubeconfig shape - only the fields NewEndpoint needs.
type kubeconfigDoc struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
			ProxyURL                 string `yaml:"proxy-url"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
			Token                 string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	CurrentContext string `yaml:"current-context"`
}

// NewEndpoint parses kc and builds an Endpoint for its current context. proxyURL, when non-empty,
// routes the transport through a SOCKS proxy (the remote-KVM tunnel internal/kvmhost holds open,
// mirroring how WithProxy stamps proxy-url for kubectl/helm); empty means dial the API server
// directly, and any proxy-url embedded in the kubeconfig is used as the fallback.
func NewEndpoint(kc []byte, proxyURL string) (*Endpoint, error) {
	var doc kubeconfigDoc
	if err := yaml.Unmarshal(kc, &doc); err != nil {
		return nil, fmt.Errorf("kubeconfig: parse: %w", err)
	}
	if len(doc.Clusters) == 0 || len(doc.Users) == 0 {
		return nil, fmt.Errorf("kubeconfig: needs at least one cluster and one user")
	}

	// Resolve the current context to a cluster+user; fall back to the first of each (the platform's
	// generated kubeconfigs are single-context, so this is belt-and-braces).
	clusterName, userName := "", ""
	for _, ctx := range doc.Contexts {
		if ctx.Name == doc.CurrentContext {
			clusterName, userName = ctx.Context.Cluster, ctx.Context.User
			break
		}
	}
	cl := doc.Clusters[0].Cluster
	for _, c := range doc.Clusters {
		if c.Name == clusterName {
			cl = c.Cluster
			break
		}
	}
	usr := doc.Users[0].User
	for _, u := range doc.Users {
		if u.Name == userName {
			usr = u.User
			break
		}
	}
	if cl.Server == "" {
		return nil, fmt.Errorf("kubeconfig: cluster has no server")
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cl.InsecureSkipTLSVerify} //nolint:gosec // honours the kubeconfig's own flag
	if cl.CertificateAuthorityData != "" {
		ca, err := base64.StdEncoding.DecodeString(cl.CertificateAuthorityData)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: decode CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("kubeconfig: no valid certificate in certificate-authority-data")
		}
		tlsCfg.RootCAs = pool
	}
	if usr.ClientCertificateData != "" && usr.ClientKeyData != "" {
		certPEM, err := base64.StdEncoding.DecodeString(usr.ClientCertificateData)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: decode client cert: %w", err)
		}
		keyPEM, err := base64.StdEncoding.DecodeString(usr.ClientKeyData)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: decode client key: %w", err)
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: client keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	transport := &http.Transport{
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if proxyURL == "" {
		proxyURL = cl.ProxyURL
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: parse proxy-url %q: %w", proxyURL, err)
		}
		transport.Proxy = http.ProxyURL(u)
	}

	return &Endpoint{Server: cl.Server, Transport: transport, Token: usr.Token}, nil
}
