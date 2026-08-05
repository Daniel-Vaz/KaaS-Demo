//go:build js

package pty

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
)

// Runner is the js/wasm stand-in for the real PTY runner. A browser has no processes to fork and no
// OS pseudo-terminal to allocate, so the whole implementation (which needs syscall.TIOCSWINSZ and
// os/exec) is compiled out and this exists only so the package's importers - internal/shell/agent
// and, through it, internal/app - still build. cmd/demo-wasm never starts the exec agent, so this
// Serve is unreachable there; the browser demo's terminal is the in-process shell.Fake instead.
type Runner struct {
	Shell        string
	WorkDir      string
	KubeProxyURL string
	Log          *slog.Logger
}

// New mirrors the real constructor's signature so callers need no build tag of their own.
func New(shellBin, workDir, kubeProxyURL string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{Shell: shellBin, WorkDir: workDir, KubeProxyURL: kubeProxyURL, Log: log}
}

// Serve always fails: there is no PTY to serve in a browser.
func (r *Runner) Serve(context.Context, *domain.Cluster, []byte, uint16, uint16, shell.Conn) error {
	return errors.New("PTY sessions are not available in a js/wasm build")
}
