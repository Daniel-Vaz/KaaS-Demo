// Package netbox registers the IP addresses a cluster occupies in NetBox, an external IPAM, and
// releases them when the cluster is deleted. Opt-in (KAAS_NETBOX_URL) and vSphere-only: KVM
// clusters own private, per-cluster libvirt networks that no other system has any business
// knowing about, whereas vSphere clusters draw from a real, shared, operator-owned subnet where
// an unrecorded address is an address someone else will hand out twice.
//
// It is a RECORDER, not an allocator: the platform still decides addresses (an external DHCP
// server in dhcp mode, internal/netpool in static mode) and NetBox is told what they are. That
// keeps cluster creation independent of NetBox's availability for the decision itself, and it is
// why every write here is an idempotent upsert keyed on the address.
//
// Records are scoped by our own tag plus a "kaas:<cluster-id>" marker in the description, so a
// deployment that already syncs vCenter into NetBox (with its own tags) keeps working and we
// only ever delete what we created.
package netbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultTag is the NetBox tag every record we create carries (KAAS_NETBOX_TAG overrides).
const DefaultTag = "kaas"

type Config struct {
	BaseURL string // e.g. https://netbox.example.internal
	// Token authenticates the API directly. When empty, one is provisioned from Username/Password
	// via NetBox's token-provisioning endpoint - a demo convenience; production would issue a
	// scoped token out-of-band and pass only that.
	Token    string
	Username string
	Password string
	Insecure bool   // accept a self-signed NetBox certificate (lab)
	Tag      string // defaults to DefaultTag
	Log      *slog.Logger
}

type Client struct {
	cfg  Config
	http *http.Client

	mu         sync.Mutex
	token      string // cached; provisioned lazily from Username/Password when Token is unset
	tagEnsured bool
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("netbox: KAAS_NETBOX_URL is required")
	}
	if cfg.Token == "" && (cfg.Username == "" || cfg.Password == "") {
		return nil, fmt.Errorf("netbox: either KAAS_NETBOX_TOKEN or KAAS_NETBOX_USERNAME + KAAS_NETBOX_PASSWORD are required")
	}
	if cfg.Tag == "" {
		cfg.Tag = DefaultTag
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("netbox: Log is required")
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // lab NetBox uses a self-signed cert
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second, Transport: tr}, token: cfg.Token}, nil
}

// IPRecord is one address we want NetBox to know about.
type IPRecord struct {
	Address     string // CIDR notation, as NetBox stores it: "172.23.252.51/24"
	DNSName     string // the VM name (or "<cluster>-cp-vip")
	Description string // carries the "kaas:<cluster-id>" marker - see Description()
	Role        string // NetBox ip-address role, e.g. "vip"; empty for a node address
}

// Description is the description every record we own carries: a machine-readable cluster-id
// marker (the delete key) plus the human name.
func Description(clusterName, clusterID string) string {
	return fmt.Sprintf("kaas:%s cluster=%s", clusterID, clusterName)
}

// EnsureIP upserts one address: created if absent, patched if present, so a re-run of a
// reconcile step converges rather than duplicating. Matching is by address - the natural key in
// an IPAM - and an existing record from another system (e.g. a vCenter sync) is updated in place
// rather than duplicated, which is what an IPAM operator would expect to see.
func (c *Client) EnsureIP(ctx context.Context, r IPRecord) error {
	if err := c.ensureTag(ctx); err != nil {
		return err
	}
	existing, err := c.findByAddress(ctx, r.Address)
	if err != nil {
		return err
	}
	body := map[string]any{
		"address":     r.Address,
		"status":      "active",
		"dns_name":    r.DNSName,
		"description": r.Description,
		"tags":        []map[string]string{{"slug": c.cfg.Tag}},
	}
	if r.Role != "" {
		body["role"] = r.Role
	}
	if existing == 0 {
		return c.do(ctx, http.MethodPost, "/api/ipam/ip-addresses/", body, nil)
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/ipam/ip-addresses/%d/", existing), body, nil)
}

// DeleteCluster removes every address we registered for a cluster. Scoped by BOTH our tag and
// the cluster-id marker, so it can never delete a record another system owns. Idempotent: an
// already-deleted record (or a cluster that never got as far as registering) is a no-op.
func (c *Client) DeleteCluster(ctx context.Context, clusterID string) error {
	// If our tag doesn't exist yet, nothing carries it, so there is nothing of ours to release -
	// and we must NOT ask anyway: NetBox rejects a filter on an unknown tag slug with a 400
	// ("Select a valid choice"), which would fail the teardown step forever. This is the state a
	// cluster is in when it never got as far as registering an address (e.g. it failed while
	// provisioning infrastructure) against a NetBox that has never seen us.
	exists, err := c.tagExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	q := url.Values{}
	q.Set("tag", c.cfg.Tag)
	q.Set("description__ic", "kaas:"+clusterID)
	q.Set("limit", "200")
	for {
		var page struct {
			Next    string `json:"next"`
			Results []struct {
				ID int `json:"id"`
			} `json:"results"`
		}
		if err := c.do(ctx, http.MethodGet, "/api/ipam/ip-addresses/?"+q.Encode(), nil, &page); err != nil {
			return err
		}
		for _, r := range page.Results {
			if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/ipam/ip-addresses/%d/", r.ID), nil, nil); err != nil {
				return err
			}
		}
		// Deleting shrinks the result set under us, so re-query from the start rather than
		// following `next` (whose offset would then skip records).
		if page.Next == "" || len(page.Results) == 0 {
			return nil
		}
	}
}

func (c *Client) findByAddress(ctx context.Context, address string) (int, error) {
	q := url.Values{}
	q.Set("address", address)
	var page struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/ipam/ip-addresses/?"+q.Encode(), nil, &page); err != nil {
		return 0, err
	}
	if len(page.Results) == 0 {
		return 0, nil
	}
	return page.Results[0].ID, nil
}

// tagExists reports whether our tag is defined in this NetBox. Filtering by a tag slug NetBox
// doesn't know is a 400, not an empty result, so every read that filters on it must check first.
func (c *Client) tagExists(ctx context.Context) (bool, error) {
	c.mu.Lock()
	done := c.tagEnsured
	c.mu.Unlock()
	if done {
		return true, nil
	}
	var page struct {
		Count int `json:"count"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/extras/tags/?slug="+url.QueryEscape(c.cfg.Tag), nil, &page); err != nil {
		return false, err
	}
	return page.Count > 0, nil
}

// ensureTag creates our tag once per process if the NetBox instance doesn't have it yet -
// otherwise the first EnsureIP would fail on an unknown tag slug. Only the WRITE path creates it:
// a teardown against a NetBox that has never seen us has nothing to release, and should not leave
// a tag behind as its only trace.
func (c *Client) ensureTag(ctx context.Context) error {
	exists, err := c.tagExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		body := map[string]any{
			"name": c.cfg.Tag, "slug": c.cfg.Tag,
			"description": "Managed by the KaaS control plane",
		}
		if err := c.do(ctx, http.MethodPost, "/api/extras/tags/", body, nil); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.tagEnsured = true
	c.mu.Unlock()
	return nil
}

// authToken returns the API token, provisioning one from Username/Password on first use.
func (c *Client) authToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	body, _ := json.Marshal(map[string]string{"username": c.cfg.Username, "password": c.cfg.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/api/users/tokens/provision/", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("netbox: provision token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("netbox: provision token: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("netbox: provision token: %w", err)
	}
	if out.Key == "" {
		return "", fmt.Errorf("netbox: provision token: response carried no key")
	}
	c.mu.Lock()
	c.token = out.Key
	c.mu.Unlock()
	c.cfg.Log.Info("netbox: provisioned an API token from username/password")
	return out.Key, nil
}

// do issues one API call, decoding a JSON response into out when non-nil. A 404 on DELETE is
// success: the record is already gone, which is what we wanted.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	tok, err := c.authToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("netbox: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if method == http.MethodDelete && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("netbox: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("netbox: %s %s: decode: %w", method, path, err)
	}
	return nil
}
