package pty

import (
	"context"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// TestSessionEnvIsScrubbed is the security regression guard for the cluster shell: the child shell
// must get an explicit allowlist, never the parent's environment. If someone re-introduces
// os.Environ(), the worker/sandbox secrets below would appear in the user's `env` output - this test
// fails first. See sessionEnv's doc comment and internal/shell.
func TestSessionEnvIsScrubbed(t *testing.T) {
	// Simulate a parent process (the worker) that holds every sensitive value in its own environment.
	secrets := map[string]string{
		"KAAS_SECRET_KEY":           "super-secret-aes-key",
		"DATABASE_URL":              "postgres://kaas:kaas@localhost:5432/kaas",
		"KAAS_SHELL_TOKEN":          "shared-bearer-token",
		"KAAS_ADMIN_PASSWORD":       "admin",
		"KAAS_SSH_PRIVATE_KEY_FILE": "/keys/id",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}

	env := sessionEnv("/work/shell/c1/sess/kubeconfig", "/work/shell/c1/sess", "demo")

	// No secret key (nor its value) may appear anywhere in the child environment.
	for k, v := range secrets {
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("secret env %q leaked into the shell session environment", k)
			}
			if strings.Contains(kv, v) {
				t.Errorf("secret value for %q leaked into the shell session environment: %q", k, kv)
			}
		}
	}

	// The environment is exactly the allowlist - nothing more, nothing less.
	got := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		got[parts[0]] = parts[1]
	}
	want := map[string]string{
		"PATH":         "/usr/local/bin:/usr/bin:/bin",
		"KUBECONFIG":   "/work/shell/c1/sess/kubeconfig",
		"HOME":         "/work/shell/c1/sess",
		"TERM":         "xterm-256color",
		"LANG":         "C.UTF-8",
		"KAAS_CLUSTER": "demo",
	}
	if len(got) != len(want) {
		t.Fatalf("session env has %d vars, want %d: %v", len(got), len(want), env)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("session env %q = %q, want %q", k, got[k], w)
		}
	}
}

// TestServeRejectsBadClusterID: the cluster ID comes from the agent handshake and names a dir under
// WorkDir, so a traversal is refused before anything touches the filesystem.
func TestServeRejectsBadClusterID(t *testing.T) {
	r := New("", t.TempDir(), "", nil)
	for _, bad := range []string{"", "..", "../../etc", "a/b", ".hidden"} {
		err := r.Serve(context.Background(), &domain.Cluster{ID: bad, Name: "x"}, []byte("kc"), 24, 80, nil)
		if err == nil || !strings.Contains(err.Error(), "not a valid cluster ID") {
			t.Errorf("Serve(ID=%q) = %v, want a cluster-ID validation error", bad, err)
		}
	}
}
