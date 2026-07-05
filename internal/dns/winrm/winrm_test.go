package winrmdns

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRunner is a scriptable psRunner: it records every script it was asked to run and returns
// whatever the test queued up, so EnsureCluster/ReleaseCluster can be exercised without a real
// WinRM endpoint.
type fakeRunner struct {
	scripts  []string
	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (f *fakeRunner) RunPSWithContext(_ context.Context, script string) (string, string, int, error) {
	f.scripts = append(f.scripts, script)
	return f.stdout, f.stderr, f.exitCode, f.err
}

func reg(f *fakeRunner) *Registrar {
	return &Registrar{
		cfg:    Config{Log: discardLog()},
		client: f,
		zone:   "kaas.example.internal",
	}
}

func TestEnsureClusterPublishesWildcard(t *testing.T) {
	f := &fakeRunner{}
	r := reg(f)
	c := &domain.Cluster{ID: "abc", Name: "dev", AppsDomain: "apps.dev.kaas.example.internal", LoadBalancerIP: "172.23.252.223"}

	if err := r.EnsureCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 1 {
		t.Fatalf("scripts run = %d, want 1", len(f.scripts))
	}
	script := f.scripts[0]
	for _, want := range []string{
		"'kaas.example.internal'",
		"'*.apps.dev'",
		"'172.23.252.223'",
		"Add-DnsServerResourceRecordA",
		"Get-DnsServerResourceRecord",
		"Remove-DnsServerResourceRecord",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// -ComputerName would trigger the PowerShell "double hop" CIM failure we hit against the real
	// DC - every cmdlet here must operate on the local (already-connected) machine implicitly.
	if strings.Contains(script, "-ComputerName") {
		t.Errorf("script must not pass -ComputerName (double-hop CIM failure):\n%s", script)
	}
}

func TestReleaseClusterIsDeleteOnly(t *testing.T) {
	f := &fakeRunner{}
	r := reg(f)
	c := &domain.Cluster{ID: "abc", Name: "dev", AppsDomain: "apps.dev.kaas.example.internal"}

	if err := r.ReleaseCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	script := f.scripts[0]
	if strings.Contains(script, "Add-DnsServerResourceRecordA") {
		t.Errorf("release script should never add a record:\n%s", script)
	}
	if !strings.Contains(script, "Remove-DnsServerResourceRecord") {
		t.Errorf("release script missing the delete:\n%s", script)
	}
	// Withdrawing an absent record must be a successful no-op. Without a terminal `exit 0` the last
	// executed statement is the Get, whose suppressed not-found error leaves $?=$false and PowerShell
	// exits 1 with empty stderr - wedging teardown of any cluster whose wildcard was never published.
	if !strings.HasSuffix(strings.TrimSpace(script), "exit 0") {
		t.Errorf("release script must end with `exit 0` so an absent-record delete succeeds:\n%s", script)
	}
}

func TestNoAppsDomainIsNoop(t *testing.T) {
	f := &fakeRunner{}
	r := reg(f)
	c := &domain.Cluster{ID: "abc", Name: "dev"} // DNS was off at admission

	if err := r.EnsureCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseCluster(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 0 {
		t.Fatalf("scripts run = %d, want 0 (no domain to publish)", len(f.scripts))
	}
}

func TestEnsureClusterFailsWithoutLoadBalancerIP(t *testing.T) {
	f := &fakeRunner{}
	r := reg(f)
	c := &domain.Cluster{ID: "abc", Name: "dev", AppsDomain: "apps.dev.kaas.example.internal"}

	if err := r.EnsureCluster(context.Background(), c); err == nil {
		t.Fatal("want an error when LoadBalancerIP is unset")
	}
	if len(f.scripts) != 0 {
		t.Fatalf("scripts run = %d, want 0", len(f.scripts))
	}
}

func TestRunPropagatesNonZeroExit(t *testing.T) {
	f := &fakeRunner{stderr: "REFUSED", exitCode: 9}
	r := reg(f)
	c := &domain.Cluster{ID: "abc", Name: "dev", AppsDomain: "apps.dev.kaas.example.internal", LoadBalancerIP: "172.23.252.223"}

	err := r.EnsureCluster(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("err = %v, want it to surface the remote stderr", err)
	}
}

func TestRelativeName(t *testing.T) {
	got, err := relativeName("*.apps.dev.kaas.example.internal", "kaas.example.internal")
	if err != nil {
		t.Fatal(err)
	}
	if got != "*.apps.dev" {
		t.Fatalf("relativeName = %q, want %q", got, "*.apps.dev")
	}
	if _, err := relativeName("*.apps.dev.other.internal", "kaas.example.internal"); err == nil {
		t.Fatal("want an error for a record outside the zone")
	}
}

func TestPsQuoteEscapesEmbeddedQuotes(t *testing.T) {
	if got := psQuote("o'brien"); got != "'o''brien'" {
		t.Fatalf("psQuote = %q", got)
	}
}
