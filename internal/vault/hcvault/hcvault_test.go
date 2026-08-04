package hcvault

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

// The exact body a real Vault returns for GET /v1/sys/auth (captured from a running server): the
// mount map is repeated at the TOP LEVEL for backwards compatibility, merged in with the standard
// response envelope, whose fields are scalars. Decoding the top level into a map of structs fails on
// the first scalar it meets - which is the bug this test exists to catch.
const sysAuthBody = `{
  "token/":    {"accessor": "auth_token_1111", "type": "token"},
  "userpass/": {"accessor": "auth_userpass_2222", "type": "userpass"},
  "ldap/":     {"accessor": "auth_ldap_3333", "type": "ldap"},
  "request_id": "6f1c9d0a-1111-2222-3333-444455556666",
  "lease_id": "",
  "renewable": false,
  "lease_duration": 0,
  "wrap_info": null,
  "warnings": null,
  "auth": null,
  "mount_type": "system",
  "data": {
    "token/":    {"accessor": "auth_token_1111", "type": "token"},
    "userpass/": {"accessor": "auth_userpass_2222", "type": "userpass"},
    "ldap/":     {"accessor": "auth_ldap_3333", "type": "ldap"}
  }
}`

func newTestClient(t *testing.T, mode, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/auth" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{
		addr: srv.URL,
		http: srv.Client(),
		set:  vault.Settings{AuthMode: mode},
	}
}

// Regression: authAccessor used to decode the top-level object, which errors on the envelope's
// scalar fields. That error propagated out of SyncAccess, so every sweep converged nothing - no
// entity, no policy, no group - while only logging "vault: sync access". Fake mode never exercises
// this path, so nothing caught it until a real Vault was wired up.
func TestAuthAccessorDecodesEnvelopeShapedResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		{"local uses the userpass mount", vault.AuthLocal, "auth_userpass_2222"},
		{"ldap uses the ldap mount", vault.AuthLDAP, "auth_ldap_3333"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, tc.mode, sysAuthBody)
			got, err := c.authAccessor(context.Background())
			if err != nil {
				t.Fatalf("authAccessor: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("accessor = %q, want %q", got, tc.want)
			}
		})
	}
}

// A mount that is not enabled is not an error: SyncAccess still writes entities, policies and groups
// and only skips the login-time alias (it guards on a non-empty accessor). Returning an error here
// would take the whole sweep down over a recoverable gap.
func TestAuthAccessorMissingMountIsNotAnError(t *testing.T) {
	const noUserpass = `{"request_id":"x","renewable":false,"data":{"token/":{"accessor":"auth_token_1111"}}}`
	c := newTestClient(t, vault.AuthLocal, noUserpass)
	got, err := c.authAccessor(context.Background())
	if err != nil {
		t.Fatalf("authAccessor: unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("accessor = %q, want empty", got)
	}
}

// recordingSink captures emitted events.
type recordingSink struct{ got []events.Event }

func (s *recordingSink) Emit(e events.Event) { s.got = append(s.got, e) }

// Regression: every emit here used to omit ClusterID, and events.cluster_id is NOT NULL with a
// foreign key to clusters - so EVERY one of them was rejected by Postgres and logged as "events:
// persist ... violates foreign key constraint", which reads like a database fault. Two costs: a
// cluster's Vault provisioning never reached its Activity tab, and the platform-scoped warnings
// (notably authAccessor's) were swallowed entirely. Cluster-scoped messages must carry the ID;
// platform-scoped ones must not reach the sink at all, because there is no platform timeline.
func TestEmitRoutesClusterScopedToEventsAndPlatformScopedToLog(t *testing.T) {
	sink := &recordingSink{}
	c := &Client{events: sink}

	c.emit("abc123", "info", "provisioned Vault path and policies for cluster \"dev\"")
	c.emit("", "warn", "vault: auth mount has no accessor")
	c.emit("", "info", "vault: provisioned the platform Vault mount")

	if len(sink.got) != 1 {
		t.Fatalf("emitted %d events, want 1 (only the cluster-scoped one)", len(sink.got))
	}
	if sink.got[0].ClusterID != "abc123" {
		t.Fatalf("ClusterID = %q, want %q - an empty ID is an FK violation, not a no-op", sink.got[0].ClusterID, "abc123")
	}
	if sink.got[0].Source != "vault" {
		t.Fatalf("Source = %q, want %q", sink.got[0].Source, "vault")
	}
}

// A nil logger is the shape newTestClient (and the API's minter client) uses, so a platform-scoped
// emit must not panic on it.
func TestPlatformScopedEmitToleratesNilLogger(t *testing.T) {
	c := &Client{}
	c.emit("", "warn", "no logger, no sink, no panic")
}
