// Command shell-agent is the cluster shell/kubectl exec agent, run as a dedicated, unprivileged
// sandbox container (deploy/Containerfile.shell + the `shell` compose service) - the isolated,
// disposable environment users' terminal sessions land in.
//
// It hosts exactly what internal/shell/agent serves: the interactive bash+kubectl PTY (/exec) and
// the request-driven kubectl seams the Workloads/Monitoring/Security pages use (/kube-exec,
// /kube-logs). Like the worker it is host-networked (the only route to cluster API servers, see
// docs/networking.md), but unlike the worker it deliberately carries NONE of the control plane's
// power: no libvirt socket, no SSH keys, no OpenTofu/Ansible/Helm, and - critically - no
// KAAS_SECRET_KEY and no DATABASE_URL. Every request already carries its own cluster kubeconfig, so
// this process needs neither the master key nor the database; not reading them here means a user who
// somehow escapes kubectl into arbitrary bash still finds nothing worth stealing on this host.
//
// Auth is the shared bearer token (KAAS_SHELL_TOKEN) the API presents; it must match the API's. It
// is read from the environment ONLY - never derived from KAAS_SECRET_KEY as the API/worker may,
// precisely so the secret key never has to be present in the sandbox. Empty disables auth (dev only,
// warned).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kvmhost"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell/agent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell/pty"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := getenv("KAAS_SHELL_LISTEN", ":8082")
	workDir := getenv("KAAS_WORK_DIR", "/work")
	token := os.Getenv("KAAS_SHELL_TOKEN")
	if token == "" {
		log.Warn("KAAS_SHELL_TOKEN unset - the cluster shell exec channel is UNAUTHENTICATED; set it (shared with the API) for real use")
	}

	// When the KVM host is remote the cluster subnets are unreachable from here too, so every kubectl
	// this sandbox runs is routed through the SOCKS tunnel the WORKER holds open (both are
	// host-networked, so they share its loopback listener). We take only the address - never the KVM
	// credentials - so the sandbox stays as unprivileged as advertised. Empty = local KVM = direct.
	kubeProxyURL := kvmhost.ProxyURLFromEnv()

	runner := pty.New(getenv("KAAS_SHELL_BIN", "bash"), workDir, kubeProxyURL, log)
	kubeExec := kubectl.NewLocalExecer(getenv("KAAS_KUBECTL_BIN", "kubectl"), workDir, kubeProxyURL)
	ag := agent.New(token, runner, kubeExec, kubeProxyURL, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Info("shell-agent sandbox starting", "addr", addr, "work_dir", workDir, "auth", token != "", "kube_proxy", kubeProxyURL)
	if err := ag.Serve(ctx, addr); err != nil {
		log.Error("shell-agent exited", "err", err)
		os.Exit(1)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
