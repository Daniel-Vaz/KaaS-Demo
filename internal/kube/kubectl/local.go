package kubectl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kubeconfig"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

// LocalExecer runs kubectl directly on the host it lives on - the worker or the shell sandbox, the
// only containers with a route to the cluster API server (see docs/networking.md). It writes each
// cluster's kubeconfig to a private per-cluster temp file under WorkDir and points kubectl at it
// with --kubeconfig.
type LocalExecer struct {
	Bin          string // "kubectl"
	WorkDir      string // base dir for per-cluster kubeconfig files
	KubeProxyURL string // SOCKS proxy to reach the API server through; "" (local KVM) = direct
}

// NewLocalExecer returns a LocalExecer with defaults filled in. kubeProxyURL is the remote-KVM
// SOCKS proxy (internal/kvmhost); empty means the cluster subnets are directly routable from here.
func NewLocalExecer(bin, workDir, kubeProxyURL string) *LocalExecer {
	if bin == "" {
		bin = "kubectl"
	}
	return &LocalExecer{Bin: bin, WorkDir: workDir, KubeProxyURL: kubeProxyURL}
}

// Run executes a one-shot kubectl command and captures stdout. A non-zero exit is returned as a
// Result (with the exit code and stderr), not a Go error - the Client turns that into a user-facing
// message; a Go error is reserved for a failure to launch kubectl at all.
func (e *LocalExecer) Run(ctx context.Context, kc []byte, clusterID string, args []string) (Result, error) {
	return e.RunInput(ctx, kc, clusterID, nil, args)
}

// RunInput is Run with bytes fed to kubectl's stdin (nil = none) - the input path for
// `kubectl create -f -`, which the per-user kubeconfig mint uses to submit a CSR. It satisfies the
// optional inputExecer the mint type-asserts (see kubectl.go); every other call goes through Run.
func (e *LocalExecer) RunInput(ctx context.Context, kc []byte, clusterID string, stdin []byte, args []string) (Result, error) {
	kcPath, err := e.writeKubeconfig(clusterID, kc)
	if err != nil {
		return Result{}, err
	}
	full := append([]string{"--kubeconfig", kcPath}, args...)
	var stderr strings.Builder
	emit := func(s string) { stderr.WriteString(s); stderr.WriteByte('\n') }
	out, err := procstream.CaptureInput(ctx, "", os.Environ(), stdin, emit, e.Bin, full...)
	res := Result{Stdout: out, Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("run kubectl: %w", err)
	}
	return res, nil
}

// Stream runs a long-lived kubectl command (kubectl logs -f) and copies its stdout to sink as it
// arrives, until the command exits, sink write fails (peer gone), or ctx is cancelled.
func (e *LocalExecer) Stream(ctx context.Context, kc []byte, clusterID string, args []string, sink kube.LogSink) error {
	kcPath, err := e.writeKubeconfig(clusterID, kc)
	if err != nil {
		return err
	}
	full := append([]string{"--kubeconfig", kcPath}, args...)
	cmd := exec.CommandContext(ctx, e.Bin, full...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &lineCollector{w: &stderr}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start kubectl logs: %w", err)
	}

	buf := make([]byte, 16*1024)
	peerGone := false
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			if werr := sink.Write(buf[:n]); werr != nil {
				peerGone = true
				_ = cmd.Process.Kill()
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	werr := cmd.Wait()
	// A cancelled context or a client that disconnected is a normal end for a follow stream.
	if peerGone || ctx.Err() != nil {
		return nil
	}
	if werr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("kubectl logs: %s", msg)
		}
		return werr
	}
	return nil
}

func (e *LocalExecer) writeKubeconfig(clusterID string, kc []byte) (string, error) {
	dir := filepath.Join(e.WorkDir, "kube", clusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Routes Workloads/Monitoring/Security kubectl calls through the KVM host when the cluster isn't
	// locally reachable; no-op when it is (see internal/kubeconfig).
	kc, err := kubeconfig.WithProxy(kc, e.KubeProxyURL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// lineCollector is a tiny io.Writer that accumulates stderr for error reporting.
type lineCollector struct{ w *strings.Builder }

func (l *lineCollector) Write(p []byte) (int, error) {
	l.w.Write(p)
	return len(p), nil
}
