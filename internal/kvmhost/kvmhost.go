// Package kvmhost describes WHERE the KVM/libvirt host is - and, when it is not the machine the
// control plane runs on, how every path that must reach the VMs gets there.
//
// The historical assumption was "the KVM host is localhost": the worker ran host-networked on the
// box that owned the libvirt socket and the per-cluster virbrN bridges, so libvirt was a unix
// socket away and the cluster subnets were directly routable (see docs/networking.md). A remote
// KVM host breaks all four of those paths at once, and each needs a different answer:
//
//	libvirt (OpenTofu)   → qemu+ssh:// instead of qemu:///system            LibvirtURI
//	SSH to VMs (Ansible) → ssh ProxyCommand through the KVM host as bastion  AnsibleSSHCommonArgs
//	API server (kubectl, → a SOCKS5 tunnel over SSH, injected into the       KubeProxyURL
//	  helm, metrics,        kubeconfig as `proxy-url` (client-go honours it,
//	  health, workloads,    so kubectl AND helm route through it unchanged)
//	  monitoring, security)
//	the shell sandbox    → the same SOCKS proxy, but it opens no tunnel and  ProxyURLFromEnv
//	                       holds no key: it shares the worker's netns
//	the nodessh sandbox  → the same bastion ProxyCommand as Ansible, built   ProxyCommand
//	  (internal/nodessh)    from a Host it constructs itself - the one
//	                        sandbox that DOES hold both keys (see below)
//
// All three derived values are empty/no-op when the host is local, so the local topology behaves
// exactly as before - remoteness is opt-in via KAAS_KVM_HOST.
//
// The single SSH identity (KAAS_KVM_SSH_KEY_FILE) is the *platform's* key to the KVM host. It is
// unrelated to KAAS_SSH_PRIVATE_KEY_FILE, the key cloud-init injects into the cluster VMs; the
// bastion hop uses the former, the final hop to the VM the latter. Two processes need BOTH and so
// construct a full Host: the worker, and the node-ssh sandbox (cmd/node-ssh-agent), which chains
// them exactly as Ansible does. Every other sandbox is credential-free and must use
// ProxyURLFromEnv instead - see its doc.
//
// Production would not tunnel SSH at all: it would peer the control plane's network with the
// hypervisor's (VPN/private link) and talk to libvirtd over TLS with a real CA, rather than
// leaning on one SSH session as the data path for every cluster.
package kvmhost

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultSocksAddr is where the worker binds the SSH SOCKS5 proxy when the KVM host is remote.
// Loopback on purpose: the worker and the shell sandbox are both host-networked, so they share
// this listener without it ever being exposed off-box.
const DefaultSocksAddr = "127.0.0.1:1080"

// Host is the KVM/libvirt host the platform provisions onto. The zero value (Addr == "") is the
// local host - the default, and the only topology before this existed.
type Host struct {
	Addr           string // hostname/IP of the KVM host; "" means local
	Port           int    // SSH port, default 22
	User           string // SSH user on the KVM host (must be able to reach libvirtd + the VM bridges)
	KeyFile        string // SSH private key for that user; required when Addr is set
	KnownHostsFile string // optional; when empty, host-key checking is DISABLED (see below)
	SocksAddr      string // where to bind the SOCKS5 proxy; default DefaultSocksAddr
	SSHBin         string // "ssh"
	uriOverride    string // KAAS_LIBVIRT_URI, wins over the derived URI

	cancel context.CancelFunc // stops the tunnel supervisor
	shared bool               // the tunnel belongs to another worker replica; we only use it (see Start)
}

// FromEnv reads the KVM host from the environment. A missing KAAS_KVM_HOST yields a local Host,
// which is the default and makes every method below a no-op.
func FromEnv() (*Host, error) {
	h := &Host{
		Addr:           strings.TrimSpace(os.Getenv("KAAS_KVM_HOST")),
		User:           getenv("KAAS_KVM_SSH_USER", "root"),
		KeyFile:        os.Getenv("KAAS_KVM_SSH_KEY_FILE"),
		KnownHostsFile: os.Getenv("KAAS_KVM_KNOWN_HOSTS_FILE"),
		SocksAddr:      getenv("KAAS_KVM_SOCKS_ADDR", DefaultSocksAddr),
		SSHBin:         getenv("KAAS_KVM_SSH_BIN", "ssh"),
		uriOverride:    os.Getenv("KAAS_LIBVIRT_URI"),
	}
	port, err := strconv.Atoi(getenv("KAAS_KVM_SSH_PORT", "22"))
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("kvmhost: invalid KAAS_KVM_SSH_PORT %q", os.Getenv("KAAS_KVM_SSH_PORT"))
	}
	h.Port = port
	if !h.Remote() {
		return h, nil
	}
	if h.KeyFile == "" {
		return nil, fmt.Errorf("kvmhost: KAAS_KVM_SSH_KEY_FILE is required when KAAS_KVM_HOST is set (%s)", h.Addr)
	}
	fi, err := os.Stat(h.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("kvmhost: KAAS_KVM_SSH_KEY_FILE %q: %w", h.KeyFile, err)
	}
	// An empty file is the shape a *missing* key takes in containers: compose bind-mounts /dev/null
	// at the key path when the host-side variable is unset, so the file exists and only its size
	// gives the mistake away. Catch it here rather than letting every reconcile die inside ssh.
	if fi.Size() == 0 {
		return nil, fmt.Errorf("kvmhost: KAAS_KVM_SSH_KEY_FILE %q is empty - set it on the host (it is bind-mounted into the worker) to reach KVM host %s", h.KeyFile, h.Addr)
	}
	return h, nil
}

// Remote reports whether the KVM host is a different machine than the one we run on.
func (h *Host) Remote() bool { return h != nil && h.Addr != "" }

// LibvirtURI is the connection URI for the OpenTofu libvirt provider. Local keeps the unix socket;
// remote dials libvirtd over SSH, which needs no libvirtd-side TLS/TCP configuration - the same
// trade the rest of this package makes. An explicit KAAS_LIBVIRT_URI always wins (escape hatch for
// a properly set up qemu+tls:// endpoint).
//
// Note what does NOT change when the host is remote: the golden images still ORIGINATE on the
// machine running OpenTofu (the worker), so KAAS_IMAGE_DIR/KAAS_BASE_IMAGE stay worker-local paths.
// They are not imported through the provider, though - StageImage uploads each one into the
// hypervisor's pool once and the module clones it there. See stage.go for why.
func (h *Host) LibvirtURI() string {
	if h.uriOverride != "" {
		return h.uriOverride
	}
	if !h.Remote() {
		return "qemu:///system"
	}
	// The query is assembled by hand rather than with url.Values.Encode(), which would percent-escape
	// the slashes in the key/known_hosts paths. Slashes are legal unescaped in a query, and the file
	// paths are far easier to read (and to eyeball in a log line) this way.
	//
	// Param name is "knownhosts" (no underscore) - that's the provider's own query key
	// (libvirt/uri/ssh.go, dmacvicar/terraform-provider-libvirt v0.8.3), NOT "known_hosts". Passing
	// the wrong name doesn't error; the provider just silently ignores it and falls back to its own
	// default (${HOME}/.ssh/known_hosts on whatever host runs OpenTofu), which then fails to open.
	params := []string{"sshauth=privkey", "keyfile=" + h.KeyFile}
	if h.KnownHostsFile != "" {
		params = append(params, "knownhosts="+h.KnownHostsFile)
	} else {
		// Shortcut, deliberate: no host-key pinning, so the first connection trusts whatever answers.
		// Production would ship a known_hosts file (KAAS_KVM_KNOWN_HOSTS_FILE) or use qemu+tls with a
		// real CA - an unauthenticated hypervisor endpoint is a fine way to hand someone your VMs.
		params = append(params, "known_hosts_verify=ignore")
	}
	return fmt.Sprintf("qemu+ssh://%s@%s/system?%s", h.User, h.hostPort(), strings.Join(params, "&"))
}

// ProxyCommand is the raw `ssh -W %h:%p …` command line that bounces one connection to a cluster VM
// through the KVM host acting as a bastion. Empty when local (VMs are directly routable).
//
// It carries its own -i: the caller sets IdentityFile to the *cluster VM* key, which is not the key
// that opens the bastion, so ProxyJump (which would reuse it) is not an option.
//
// Two consumers, wanting it in different shapes: AnsibleSSHCommonArgs wraps it for Ansible's
// ansible_ssh_common_args, and internal/nodessh/sshpty passes it straight to ssh as a single
// -o ProxyCommand=<this> argv element. The latter is why this returns the command UNQUOTED - it is
// never interpreted by a shell there, so quoting it would put literal quotes inside the option.
func (h *Host) ProxyCommand() string {
	if !h.Remote() {
		return ""
	}
	return strings.Join(append([]string{h.SSHBin, "-W", "%h:%p"}, h.sshOpts()...), " ")
}

// AnsibleSSHCommonArgs is the value for Ansible's ansible_ssh_common_args: a ProxyCommand that
// bounces every VM connection through the KVM host. Empty when local.
func (h *Host) AnsibleSSHCommonArgs() string {
	proxy := h.ProxyCommand()
	if proxy == "" {
		return ""
	}
	return fmt.Sprintf("-o ProxyCommand=%q", proxy)
}

// KubeProxyURL is the SOCKS5 proxy every platform-side kubectl/helm invocation routes through, to
// be stamped into the cluster's kubeconfig as `proxy-url` (see internal/kubeconfig). Empty when
// local, which leaves kubeconfigs untouched and every call direct.
func (h *Host) KubeProxyURL() string {
	if !h.Remote() {
		return ""
	}
	return "socks5://" + h.SocksAddr
}

// ProxyURLFromEnv is KubeProxyURL for a process that consumes the tunnel without owning it - the
// shell sandbox (cmd/shell-agent), which is host-networked alongside the worker and so shares its
// loopback listener. It reads only KAAS_KVM_HOST (is there a tunnel?) and KAAS_KVM_SOCKS_ADDR
// (where?), never the SSH key or user: that sandbox is credential-free by design and must not be
// able to construct a Host. It must agree with Host.KubeProxyURL on the address, hence the shared
// default - a sandbox that guessed differently would leave the Terminal talking to nothing.
//
// The node-ssh sandbox is the deliberate exception and calls FromEnv: it cannot be credential-free,
// because SSHing to a VM needs the VM key, and chaining through a remote hypervisor needs the
// bastion key too. It buys its containment a different way - the only process it ever starts is ssh
// itself, never a shell. See internal/nodessh.
func ProxyURLFromEnv() string {
	addr := strings.TrimSpace(os.Getenv("KAAS_KVM_SOCKS_ADDR"))
	if addr == "" && strings.TrimSpace(os.Getenv("KAAS_KVM_HOST")) != "" {
		addr = DefaultSocksAddr
	}
	if addr == "" {
		return ""
	}
	return "socks5://" + addr
}

// Start brings up the SOCKS5 tunnel to the KVM host and supervises it until ctx is cancelled,
// restarting it if the SSH session drops (a hypervisor reboot, a flaky link). It blocks until the
// listener accepts a connection, so the first reconcile can't race a not-yet-open tunnel. No-op
// when the host is local.
//
// It shells out to ssh rather than dialling with x/crypto/ssh + a hand-rolled SOCKS server: ssh
// already does keepalives, key handling and reconnect semantics, and the worker image ships it.
func (h *Host) Start(ctx context.Context, log *slog.Logger) error {
	if !h.Remote() {
		return nil
	}
	// Another worker replica on this host may already hold the tunnel: the workers are
	// host-networked (docs/networking.md), so they share one network namespace and one loopback,
	// and only the first can bind SocksAddr. The tunnel is a shared, stateless route to the
	// hypervisor - nothing about it is per-replica - so a second worker simply uses the one that is
	// already up rather than dying on "address already in use". It also does not own it: Stop
	// leaves it alone, so a replica shutting down can't cut the tunnel out from under its peers.
	// (This is the same sharing the shell sandbox already relies on - see ProxyURLFromEnv.)
	if listening(h.SocksAddr) {
		h.shared = true
		log.Info("reusing the SOCKS tunnel already open on this host", "socks", h.SocksAddr, "host", h.Addr)
		return nil
	}
	if _, err := exec.LookPath(h.SSHBin); err != nil {
		return fmt.Errorf("kvmhost: %q not found - the worker image must carry an ssh client to reach a remote KVM host: %w", h.SSHBin, err)
	}
	ctx, h.cancel = context.WithCancel(ctx)
	go h.supervise(ctx, log)
	if err := waitListening(ctx, h.SocksAddr, 30*time.Second); err != nil {
		h.Stop()
		return fmt.Errorf("kvmhost: SOCKS tunnel to %s did not come up: %w", h.Addr, err)
	}
	log.Info("kvm host tunnel up", "host", h.Addr, "socks", h.SocksAddr, "libvirt_uri", h.LibvirtURI())
	return nil
}

// Stop tears the tunnel down. Safe on a local host and safe to call twice.
func (h *Host) Stop() {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}

// supervise runs `ssh -N -D <socks>` in a restart loop. Each run is expected to be long-lived; an
// exit means the link died, so we back off briefly and redial rather than leaving every kubectl in
// the process with a dead proxy.
func (h *Host) supervise(ctx context.Context, log *slog.Logger) {
	const backoff = 3 * time.Second
	for ctx.Err() == nil {
		args := append([]string{"-N", "-T", "-D", h.SocksAddr}, h.sshOpts()...)
		cmd := exec.CommandContext(ctx, h.SSHBin, args...)
		cmd.Stdout, cmd.Stderr = logWriter{log, "stdout"}, logWriter{log, "stderr"}
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		log.Warn("kvm host tunnel exited - reconnecting", "host", h.Addr, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// sshOpts are the flags shared by the SOCKS tunnel and the Ansible ProxyCommand: which key, which
// user@host, and how (not) to verify the host key. BatchMode keeps ssh from ever blocking on a
// prompt - a reconcile step must fail fast, not hang forever waiting for a passphrase.
func (h *Host) sshOpts() []string {
	opts := []string{
		"-i", h.KeyFile,
		"-p", strconv.Itoa(h.Port),
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	if h.KnownHostsFile != "" {
		opts = append(opts, "-o", "UserKnownHostsFile="+h.KnownHostsFile, "-o", "StrictHostKeyChecking=yes")
	} else {
		// Matches LibvirtURI's known_hosts_verify=ignore - same shortcut, same production caveat.
		opts = append(opts, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	}
	return append(opts, h.User+"@"+h.Addr)
}

func (h *Host) hostPort() string {
	if h.Port == 0 || h.Port == 22 {
		return h.Addr
	}
	return net.JoinHostPort(h.Addr, strconv.Itoa(h.Port))
}

// listening reports whether something already accepts on addr - i.e. a peer worker replica sharing
// this host's network namespace has the SOCKS tunnel open (see Start).
func listening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitListening polls addr until something accepts, ctx is cancelled, or timeout elapses.
func waitListening(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// logWriter forwards ssh's output into the platform log (ssh writes its diagnostics to stderr).
type logWriter struct {
	log    *slog.Logger
	stream string
}

func (w logWriter) Write(p []byte) (int, error) {
	if line := strings.TrimSpace(string(p)); line != "" {
		w.log.Info("kvm host tunnel", w.stream, line)
	}
	return len(p), nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
