// Package procstream runs external tools (tofu, ansible-playbook) while streaming their
// stdout/stderr line-by-line to an emit callback - so provisioning output shows up in the
// per-cluster event timeline. Both provision/tofu and config/ansible use it.
package procstream

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// CaptureInput is Capture with the given bytes fed to the command's stdin (nil = no stdin). Used by
// the kube exec agent to run `kubectl create -f -` for the per-user kubeconfig mint.
func CaptureInput(ctx context.Context, dir string, env []string, stdin []byte, emit func(string), bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	w := &lineWriter{emit: emit}
	cmd.Stdout = &out
	cmd.Stderr = w
	err := cmd.Run()
	w.flush()
	return out.Bytes(), err
}

// Run executes bin with args in dir, streaming combined stdout+stderr lines to emit.
func Run(ctx context.Context, dir string, env []string, emit func(string), bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	w := &lineWriter{emit: emit}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	w.flush()
	return err
}

// Capture runs bin and returns its stdout bytes, streaming stderr lines to emit.
func Capture(ctx context.Context, dir string, env []string, emit func(string), bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	w := &lineWriter{emit: emit}
	cmd.Stdout = &out
	cmd.Stderr = w
	err := cmd.Run()
	w.flush()
	return out.Bytes(), err
}

// lineWriter splits writes into lines and calls emit for each non-empty one.
type lineWriter struct {
	emit func(string)
	buf  bytes.Buffer
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil { // partial line; keep it buffered
			l.buf.Reset()
			l.buf.WriteString(line)
			break
		}
		if s := strings.TrimRight(line, "\r\n"); s != "" {
			l.emit(s)
		}
	}
	return len(p), nil
}

func (l *lineWriter) flush() {
	if s := strings.TrimRight(l.buf.String(), "\r\n"); s != "" {
		l.emit(s)
	}
	l.buf.Reset()
}
