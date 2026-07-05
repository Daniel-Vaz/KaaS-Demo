package nodessh

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
)

// Fake is the in-process node terminal used in fake mode (make up-fake): no ssh, no VM - it drives
// the shared terminal Emulator (internal/shell) and answers a small set of Linux commands
// synthesized from the node's own identity. This keeps the Nodes-tab SSH button fully demoable with
// no KVM, mirroring every other fake seam. Like the fake shell it spawns no process, so the
// distroless API image stays shell-free.
type Fake struct{}

// NewFake returns the fake node-ssh backend.
func NewFake() *Fake { return &Fake{} }

func (f *Fake) Serve(ctx context.Context, c *domain.Cluster, n *domain.Node, term shell.Conn) error {
	return (&shell.Emulator{
		Term:   term,
		Banner: fakeBanner(c, n),
		Prompt: func() string { return fakePrompt(n) },
		Render: func(line string) (string, bool) {
			out := renderCommand(c, n, line)
			if out == cmdClear {
				return "", true
			}
			return out, false
		},
	}).Run(ctx)
}

// cmdClear is the sentinel renderCommand returns for the `clear` builtin.
const cmdClear = "\x00clear\x00"

func fakePrompt(n *domain.Node) string {
	// green kaas@host, blue cwd - an ordinary root-capable login shell prompt.
	return fmt.Sprintf("\x1b[1;32mkaas@%s\x1b[0m:\x1b[1;34m~\x1b[0m$ ", n.VMName)
}

func fakeBanner(c *domain.Cluster, n *domain.Node) string {
	role := "worker"
	if n.Role == domain.RoleControlPlane {
		role = "control-plane"
	}
	return fmt.Sprintf(
		"KaaS demo - simulated SSH session to %s (%s node of cluster %q) as kaas@%s.\r\n"+
			"Fake mode: output is synthesized from control-plane state, not a live machine;\r\n"+
			"a real ssh session to a real node runs in `make up`. Type `help` for modeled commands.\r\n\r\n",
		n.VMName, role, c.Name, n.IP)
}

// renderCommand produces the fake response for one command line, using \n line endings (the Emulator
// converts to \r\n). It returns cmdClear for the `clear` builtin. Only a handful of read-only
// diagnostic commands are modeled - the ones a write-role user reaches for when a node misbehaves.
func renderCommand(c *domain.Cluster, n *domain.Node, line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "help":
		return fakeHelp()
	case "clear":
		return cmdClear
	case "exit", "logout":
		return "This is a simulated SSH session - close the terminal to disconnect."
	case "hostname":
		return n.VMName
	case "whoami":
		return "kaas"
	case "id":
		return "uid=1000(kaas) gid=1000(kaas) groups=1000(kaas),27(sudo)"
	case "pwd":
		return "/home/kaas"
	case "uname":
		if hasArg(fields[1:], "-a") {
			return fmt.Sprintf("Linux %s 6.8.0-45-generic #45-Ubuntu SMP x86_64 GNU/Linux", n.VMName)
		}
		return "Linux"
	case "uptime":
		return fmt.Sprintf(" %s up %s,  1 user,  load average: 0.08, 0.03, 0.01",
			time.Now().Format("15:04:05"), fakeUptime(c))
	case "date":
		return time.Now().Format("Mon Jan  2 15:04:05 MST 2006")
	case "df":
		return fakeDF()
	case "free":
		return fakeFree()
	case "systemctl":
		return fakeSystemctl(fields[1:])
	case "journalctl":
		return "-- No entries --  (journal reads are modeled only as an empty tail in fake mode)"
	case "sudo":
		if len(fields) > 1 {
			return renderCommand(c, n, strings.Join(fields[1:], " ")) // passwordless sudo - just run it
		}
		return "usage: sudo <command>"
	case "ls":
		return "snap"
	case "cat", "echo":
		return notModeled(line)
	default:
		return notModeled(fields[0])
	}
}

func notModeled(cmd string) string {
	return fmt.Sprintf("%s: not modeled in the fake node shell - a real ssh session runs in `make up`. Type `help`.",
		strings.Fields(cmd + " ")[0])
}

func fakeHelp() string {
	return strings.Join([]string{
		"Modeled commands (fake node SSH):",
		"  hostname / whoami / id / pwd    identity",
		"  uname -a                        kernel",
		"  uptime / date                   liveness",
		"  df -h / free -h                 disk & memory",
		"  systemctl status kubelet        node agent state",
		"  clear                           clear the screen",
		"Anything else prints a note - a real shell on the node runs in `make up`.",
	}, "\n")
}

func fakeSystemctl(args []string) string {
	unit := "kubelet"
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && a != "status" && a != "is-active" && a != "show" {
			unit = a
		}
	}
	if hasArg(args, "is-active") {
		return "active"
	}
	return strings.Join([]string{
		fmt.Sprintf("● %s.service - kubelet: The Kubernetes Node Agent", unit),
		fmt.Sprintf("     Loaded: loaded (/usr/lib/systemd/system/%s.service; enabled; preset: enabled)", unit),
		"     Active: \x1b[1;32mactive (running)\x1b[0m since Fri 2026-07-17 09:14:22 UTC",
		"   Main PID: 1188 (kubelet)",
		"      Tasks: 14 (limit: 4915)",
		"     Memory: 42.8M",
		"        CPU: 3.204s",
	}, "\n")
}

func fakeDF() string {
	return table([][]string{
		{"Filesystem", "Size", "Used", "Avail", "Use%", "Mounted on"},
		{"/dev/vda1", "20G", "6.1G", "13G", "33%", "/"},
		{"tmpfs", "1.9G", "0", "1.9G", "0%", "/dev/shm"},
		{"/dev/vda15", "105M", "6.1M", "99M", "6%", "/boot/efi"},
	})
}

func fakeFree() string {
	return table([][]string{
		{"", "total", "used", "free", "shared", "buff/cache", "available"},
		{"Mem:", "3.8Gi", "1.2Gi", "1.4Gi", "18Mi", "1.2Gi", "2.4Gi"},
		{"Swap:", "0B", "0B", "0B", "", "", ""},
	})
}

// fakeUptime renders time since the cluster was created in the coarse `up` form (e.g. "12 min",
// "3:05"), close enough to real uptime output for a demo.
func fakeUptime(c *domain.Cluster) string {
	d := time.Since(c.CreatedAt)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d sec", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d:%02d", int(d.Hours()), int(d.Minutes())%60)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// table renders rows (first row is the header) as space-padded columns, like df/free output.
func table(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, cell := range r {
			if i == len(r)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
