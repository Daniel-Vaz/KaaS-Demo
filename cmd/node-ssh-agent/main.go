// Command node-ssh-agent is the node SSH exec agent, run as a dedicated, unprivileged sandbox
// container (deploy/Containerfile.nodessh + the `nodessh` compose service) - the isolated
// environment users' node SSH sessions land in.
//
// It hosts exactly what internal/nodessh/agent serves: GET /node-ssh, which opens one `ssh` session
// as the kaas user on a caller-named cluster VM (internal/nodessh/sshpty). Like the shell agent it is
// host-networked (the only route to the cluster subnets, see docs/networking.md).
//
// Unlike the shell agent it is NOT credential-free - and that difference is deliberate. Reaching a
// cluster VM needs the platform's VM SSH key (KAAS_SSH_PRIVATE_KEY_FILE), and chaining through a
// remote hypervisor needs the bastion key too (KAAS_KVM_SSH_KEY_FILE); both are mounted here. What
// keeps this safe is not the absence of secrets but the absence of a shell: this binary starts only
// ssh, never bash, so there is no session from which to read the key it holds. That is why it is a
// separate binary from the shell agent rather than the same one behind a flag - see internal/nodessh.
//
// It carries NONE of the control plane's other power: no libvirt socket, no OpenTofu/Ansible/Helm,
// no KAAS_SECRET_KEY, no DATABASE_URL. Auth is the shared bearer token (KAAS_NODE_SSH_TOKEN) the API
// presents; it must match the API's, is read from the environment ONLY, and empty disables auth (dev
// only, warned).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/Daniel-Vaz/KaaS-demo/internal/kvmhost"
	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh/agent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh/sshpty"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := getenv("KAAS_NODE_SSH_LISTEN", ":8084")
	workDir := getenv("KAAS_WORK_DIR", "/work")
	token := os.Getenv("KAAS_NODE_SSH_TOKEN")
	if token == "" {
		log.Warn("KAAS_NODE_SSH_TOKEN unset - the node SSH channel is UNAUTHENTICATED; set it (shared with the API) for real use")
	}

	sshUser := getenv("KAAS_SSH_USER", "kaas")
	keyFile := os.Getenv("KAAS_SSH_PRIVATE_KEY_FILE")
	if keyFile == "" {
		log.Error("KAAS_SSH_PRIVATE_KEY_FILE unset - node SSH cannot reach any VM; set it (the platform key cloud-init injects)")
		os.Exit(1)
	}

	// This sandbox is the one that holds BOTH keys (see the package doc), so it constructs a full
	// kvmhost.Host: the VM hop uses KAAS_SSH_PRIVATE_KEY_FILE, and when the hypervisor is remote the
	// bastion hop uses KAAS_KVM_SSH_KEY_FILE. ProxyCommand() is empty when the host is local.
	kvm, err := kvmhost.FromEnv()
	if err != nil {
		log.Error("kvm host config", "err", err)
		os.Exit(1)
	}

	runner := sshpty.New(getenv("KAAS_SSH_BIN", "ssh"), sshUser, keyFile, kvm.ProxyCommand(), workDir, log)
	ag := agent.New(token, runner, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Info("node-ssh-agent sandbox starting", "addr", addr, "work_dir", workDir, "auth", token != "",
		"ssh_user", sshUser, "remote_kvm", kvm.Remote())
	if err := ag.Serve(ctx, addr); err != nil {
		log.Error("node-ssh-agent exited", "err", err)
		os.Exit(1)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
