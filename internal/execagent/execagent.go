// Package execagent resolves WHICH exec agent an API replica talks to.
//
// The API cannot reach cluster VMs (docs/networking.md), so every real cluster interaction - the
// Terminal's PTY, the Workloads page's kubectl calls, Monitoring's PromQL, Security's Trivy reads -
// is forwarded to a host-networked exec agent (cmd/shell-agent, the `shell` sandbox). That agent is
// horizontally scalable by construction: it keeps NO state between requests (each request carries
// its own kubeconfig, each session lives and dies with its connection), so any instance can serve
// any request and no sticky routing is needed.
//
// What was missing was a way to address more than one. KAAS_SHELL_AGENT_ADDR takes a
// comma-separated list, and a Pool spreads sessions over it round-robin and fails over to the next
// instance when one won't answer - so an agent that dies takes no user sessions down with it
// beyond the ones it was serving.
package execagent

import (
	"strings"
	"sync/atomic"
)

// DefaultAddr is the single shell agent every real-mode deployment has: the host-networked sandbox
// reachable from the API container at Podman's host alias.
const DefaultAddr = "host.containers.internal:8082"

// DefaultNodeSSHAddr is the analogous default for the node-ssh sandbox (internal/nodessh), a
// SEPARATE host-networked container on its own port so it can hold the SSH key without being
// reachable through the shell agent's bash PTY.
const DefaultNodeSSHAddr = "host.containers.internal:8084"

// Pool is a set of interchangeable exec-agent addresses.
type Pool struct {
	addrs []string
	next  atomic.Uint64
}

// NewPool parses a comma-separated address list ("host:8082,host:8083"). An empty spec yields the
// single default shell agent, so the common one-agent deployment needs no configuration.
func NewPool(spec string) *Pool { return NewPoolDefault(spec, DefaultAddr) }

// NewPoolDefault is NewPool with a caller-chosen fallback for the empty spec - the node-ssh pool
// uses it so its default lands on DefaultNodeSSHAddr rather than the shell agent's port.
func NewPoolDefault(spec, def string) *Pool {
	var addrs []string
	for _, a := range strings.Split(spec, ",") {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		addrs = []string{def}
	}
	return &Pool{addrs: addrs}
}

// Addrs returns every agent address, in configuration order.
func (p *Pool) Addrs() []string { return p.addrs }

// Candidates returns all agent addresses, rotated so successive calls start at successive
// instances. Callers walk the list in order and stop at the first that answers: the first entry is
// the round-robin choice, the rest are the failover order.
func (p *Pool) Candidates() []string {
	if len(p.addrs) == 1 {
		return p.addrs
	}
	start := int(p.next.Add(1)-1) % len(p.addrs)
	out := make([]string, 0, len(p.addrs))
	out = append(out, p.addrs[start:]...)
	out = append(out, p.addrs[:start]...)
	return out
}
