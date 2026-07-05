package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A tiny read-only Proxmox API client, used ONLY by ImageAvailable - provisioning itself goes
// through OpenTofu, which holds its own session. It deliberately does not pull in a full Proxmox SDK
// (a new dependency) for the one GET the reconciler's rolling-OS preflight needs. It supports both
// auth methods the provisioner does: an API token (a header) or a username/password (a login ticket,
// carried as a cookie). Only the ticket cookie is needed for a GET; the CSRF token guards writes,
// which this client never does.

type apiClient struct {
	base   string // "<endpoint>/api2/json"
	http   *http.Client
	header string // "Authorization: PVEAPIToken=…" for token auth
	cookie string // "PVEAuthCookie=…" for ticket auth
}

// client builds an apiClient, performing a ticket login first when using username/password auth.
func (p *Provisioner) client(ctx context.Context) (*apiClient, error) {
	base := strings.TrimRight(p.cfg.Endpoint, "/") + "/api2/json"
	hc := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: p.cfg.Insecure}, //nolint:gosec // lab opt-in, mirrors KAAS_PROXMOX_INSECURE
		},
	}
	c := &apiClient{base: base, http: hc}
	if t := strings.TrimSpace(p.cfg.APIToken); t != "" {
		c.header = "PVEAPIToken=" + t
		return c, nil
	}
	ticket, err := login(ctx, hc, base, p.cfg.Username, p.cfg.Password)
	if err != nil {
		return nil, fmt.Errorf("proxmox: login as %s: %w", p.cfg.Username, err)
	}
	c.cookie = "PVEAuthCookie=" + ticket
	return c, nil
}

// login exchanges username/password for a session ticket.
func login(ctx context.Context, hc *http.Client, base, username, password string) (string, error) {
	form := url.Values{"username": {username}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/access/ticket", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("access/ticket: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data struct {
			Ticket string `json:"ticket"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Data.Ticket == "" {
		return "", fmt.Errorf("access/ticket: no ticket in response")
	}
	return out.Data.Ticket, nil
}

// templateExists reports whether node has a VM template with the given name.
func (c *apiClient) templateExists(ctx context.Context, node, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/nodes/"+url.PathEscape(node)+"/qemu", nil)
	if err != nil {
		return false, err
	}
	if c.header != "" {
		req.Header.Set("Authorization", c.header)
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET nodes/%s/qemu: HTTP %d: %s", node, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data []struct {
			Name     string `json:"name"`
			Template int    `json:"template"` // 1 for a template
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, err
	}
	for _, vm := range out.Data {
		if vm.Name == name && vm.Template == 1 {
			return true, nil
		}
	}
	return false, nil
}
