// Package pty runs a real interactive bash+kubectl PTY for a cluster and bridges it to a
// shell.Conn. It is the worker-side engine behind the exec agent: it runs where the cluster's API
// server is actually reachable - the host-networked worker (see docs/networking.md) - writing the
// cluster's admin kubeconfig to a per-session temp dir and pointing kubectl at it via KUBECONFIG.
//
// Isolation: the session shell is confined two ways, in depth. (1) It is meant to run in the
// dedicated, unprivileged shell-agent sandbox (deploy/Containerfile.shell + the `shell` compose
// service), which carries only bash+kubectl and none of the worker's secrets, keys, libvirt socket
// or DB access - so there is nothing sensitive on its filesystem to reach. (2) Regardless of where
// the agent runs, the child shell gets a scrubbed, allowlisted environment (sessionEnv) rather than
// the parent's - so a stray secret in the process env (e.g. under `make run-worker`) can never leak
// via `env` - and runs in its own process group so backgrounded processes are killed with the
// session. Still stubbed (and marked "production would…"): a per-session ephemeral pod, an
// RBAC-scoped kubeconfig for full-access users (today they get cluster-admin), seccomp, and session
// audit. See internal/shell for the seam overview.
package pty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/creack/pty"
)

// Runner starts PTY sessions. Shell defaults to "bash"; WorkDir is the base for per-session temp
// dirs holding the kubeconfig and rc file.
type Runner struct {
	Shell        string
	WorkDir      string
	KubeProxyURL string // SOCKS proxy to reach the API server through; "" (local KVM) = direct
	Log          *slog.Logger
}

// New returns a Runner with defaults filled in. kubeProxyURL is the remote-KVM SOCKS proxy
// (internal/kvmhost); empty means the cluster subnets are directly routable from here.
func New(shellBin, workDir, kubeProxyURL string, log *slog.Logger) *Runner {
	if shellBin == "" {
		shellBin = "bash"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{Shell: shellBin, WorkDir: workDir, KubeProxyURL: kubeProxyURL, Log: log}
}

// Serve runs an interactive shell for the cluster, bridging PTY <-> term until the shell exits, the
// terminal closes, or ctx is cancelled. rows/cols set the initial window size (the client resizes
// again as soon as it measures its viewport).
func (r *Runner) Serve(ctx context.Context, c *domain.Cluster, kc []byte, rows, cols uint16, term shell.Conn) error {
	if len(kc) == 0 {
		return fmt.Errorf("pty: no kubeconfig for cluster %s yet - reconnect once it is fully Ready", c.Name)
	}
	// Routes the session's kubectl through the KVM host when the cluster isn't locally reachable;
	// no-op when it is (see internal/kubeconfig).
	kc, err := kubeconfig.WithProxy(kc, r.KubeProxyURL)
	if err != nil {
		return fmt.Errorf("pty: %w", err)
	}
	// A per-session dir (WorkDir/shell/<cluster-id>/<session-id>) that is written once and then LEFT
	// IN PLACE for the life of the session - deliberately never removed on the way out. Two design
	// rules are load-bearing here, and both have drawn a bug before:
	//
	//   1. Unique per session, not shared per cluster. Overlapping sessions on one cluster (a second
	//      browser tab, or the portal's Tabs unmounting/remounting the Terminal panel so a new session
	//      starts while the old one is still tearing down) must not share a kubeconfig file - a shared
	//      file gets truncate-then-written under a live reader, and can mix an admin credential with a
	//      read-only viewer one across group-mates.
	//   2. Never delete the dir on disconnect. Reconnects overlap, so removing this dir when Serve
	//      returns races the *other* live session and can delete the kubeconfig out from under the
	//      terminal the user is actively typing into - kubectl then finds no file and falls back to
	//      http://localhost ("connection refused ... localhost"), stuck until the session is restarted.
	//      This is the exact failure both a random-temp-dir+RemoveAll and a per-session dir+RemoveAll
	//      attempt reintroduced. The files are small and live on the worker's /work volume alongside
	//      the reconciler's artifacts; they are reclaimed when the cluster (and its work dir) is torn
	//      down, not per session.
	dir := filepath.Join(r.WorkDir, "shell", c.ID, sessionID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pty: session dir: %w", err)
	}

	kcPath := filepath.Join(dir, "kubeconfig")
	if err := writeFileAtomic(kcPath, kc, 0o600); err != nil {
		return fmt.Errorf("pty: write kubeconfig: %w", err)
	}
	rcPath := filepath.Join(dir, "bashrc")
	if err := writeFileAtomic(rcPath, []byte(bashrc(c, kcPath)), 0o600); err != nil {
		return fmt.Errorf("pty: write rcfile: %w", err)
	}

	if rows == 0 {
		rows = shell.DefaultRows
	}
	if cols == 0 {
		cols = shell.DefaultCols
	}

	cmd := exec.CommandContext(ctx, r.Shell, "--rcfile", rcPath, "-i")
	cmd.Dir = dir
	// A scrubbed, allowlisted environment - NOT the parent's. The parent (worker or shell-agent
	// sandbox) may hold secrets in its env (KAAS_SECRET_KEY, DATABASE_URL, the shell token, SSH
	// material); inheriting os.Environ() would hand them to the user via a plain `env`. sessionEnv
	// passes only what an interactive kubectl shell needs.
	cmd.Env = sessionEnv(kcPath, dir, c.Name)

	// pty.StartWithSize sets SysProcAttr.Setsid (a new session + controlling tty), which also makes
	// the child the leader of a NEW process group with pgid == pid. We must NOT also set Setpgid
	// ourselves: setpgid(2) rejects a session leader with EPERM ("fork/exec: operation not
	// permitted"), which would break every session (a bug this code shipped once). We reap that
	// process group below on the way out - best-effort cleanup of the shell and any children still in
	// its group. (Interactive bash's job control puts `&` background jobs in their own groups, which
	// this won't catch; the sandbox's pids_limit and its disposable tmpfs bound those anyway.)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return fmt.Errorf("pty: start %s: %w", r.Shell, err)
	}
	defer func() { _ = f.Close() }()
	if cmd.Process != nil {
		pgid := cmd.Process.Pid // setsid made the child a group leader, so pgid == pid
		defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	}

	// term -> PTY: stdin bytes and resize control. Runs until the terminal closes; then closing the
	// PTY makes the read loop below unblock.
	go func() {
		for {
			isText, data, err := term.ReadMessage()
			if err != nil {
				_ = f.Close()
				return
			}
			if isText {
				var ctrl shell.Control
				if json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "resize" {
					_ = pty.Setsize(f, &pty.Winsize{Rows: ctrl.Rows, Cols: ctrl.Cols})
				}
				continue
			}
			if _, err := f.Write(data); err != nil {
				return
			}
		}
	}()

	// PTY -> term: stream shell output until EOF (shell exit or PTY closed).
	buf := make([]byte, 16*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if werr := term.WriteBinary(buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	code := exitCode(cmd.Wait())
	_ = shell.WriteControl(term, shell.Control{Type: "exit", Code: code})
	return nil
}

// sessionEnv builds the child shell's environment from scratch - an explicit allowlist, never the
// parent's os.Environ(). This is the load-bearing "no confidential data" control: whatever secrets
// the host process holds (KAAS_SECRET_KEY, DATABASE_URL, KAAS_SHELL_TOKEN, SSH key paths, admin
// password) are simply absent from the user's shell, so `env`/`printenv` reveal nothing. Only what
// an interactive kubectl session needs is passed: a fixed PATH (so tools still resolve without
// leaking a customized parent PATH), the per-session KUBECONFIG and HOME, a terminal type, a locale,
// and the cosmetic cluster name used by the prompt banner. Add to this list deliberately - never
// re-introduce os.Environ().
func sessionEnv(kcPath, home, clusterName string) []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"KUBECONFIG=" + kcPath,
		"HOME=" + home,
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"KAAS_CLUSTER=" + clusterName,
	}
}

// bashrc is the interactive rc file: it pins kubectl to the session kubeconfig, sets a recognizable
// prompt, adds a `k` alias, wires up kubectl tab-completion (for both `kubectl` and `k`), and prints
// a one-line hint. HOME is the temp dir so `\w` shows `~`.
func bashrc(c *domain.Cluster, kcPath string) string {
	return fmt.Sprintf(`export KUBECONFIG=%q
export PS1='\[\e[1;32m\]kaas@%s\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]$ '
alias k=kubectl
alias ll='ls -la'
# Tab-completion: load bash-completion's helpers (kubectl's script relies on __ltrim_colon_completions
# and friends), then kubectl's own completion, and bind it to the k alias too.
if [ -r /usr/share/bash-completion/bash_completion ]; then . /usr/share/bash-completion/bash_completion; fi
source <(kubectl completion bash)
complete -o default -F __start_kubectl k
echo "KaaS shell - cluster %s (k8s v%s). kubectl is configured (tab-completion on); try: kubectl get nodes"
`, kcPath, c.Name, c.Name, c.K8sVersion)
}

// sessionID returns a short random identifier so concurrent sessions on the same cluster never
// share a directory (and so a kubeconfig/cache file).
func sessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeFileAtomic writes data to a temp file in the same dir and renames it into place, so a reader
// (kubectl loading the kubeconfig) never observes a partially written or truncated file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}
