package kvmhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// StageImage makes the golden image at localPath available as a volume named name in the
// hypervisor's storage pool, uploading it only if it isn't already there. It is the remote-KVM
// replacement for letting the OpenTofu libvirt provider import the image itself.
//
// Why this exists rather than just passing the path to the provider: the provider's libvirt_volume
// declares no Timeouts block, so its create inherits the plugin SDK's fixed 20-minute default. A
// multi-GB qcow2 over a slow link to the hypervisor simply cannot finish inside that, and the
// timeout is not reachable from HCL - the import would fail forever, restarting from zero each try.
// Staging out-of-band moves the transfer under our own (configurable) reconcile budget, and - the
// bigger win - makes it happen ONCE PER IMAGE instead of once per cluster: every later cluster
// clones from the staged volume and provisions with no upload at all.
//
// Idempotent, which is what lets the reconcile loop simply retry it: an image whose staged size
// already matches the local file is left alone.
//
// Shortcuts, deliberate: the transfer is a plain `cat` over SSH into the pool's directory (so this
// assumes a dir-backed pool, which is what libvirt's `default` is) and is not resumable - an
// interrupted upload restarts. A real platform would keep golden images in an image registry the
// hypervisors pull from, not push them from the control plane.
func (h *Host) StageImage(ctx context.Context, pool, name, localPath string, emit func(string)) error {
	if !h.Remote() {
		return fmt.Errorf("kvmhost: StageImage called with no remote KVM host")
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("kvmhost: golden image %q: %w", localPath, err)
	}
	size := fi.Size()

	staged, err := h.stagedSize(ctx, pool, name)
	if err != nil {
		return err
	}
	if staged == size {
		emit(fmt.Sprintf("golden image %s already staged on KVM host %s (%s) - skipping upload", name, h.Addr, humanSize(size)))
		return nil
	}
	if staged >= 0 {
		// A size mismatch means a previous upload was cut short (or the image was rebuilt); re-upload
		// over it rather than booting VMs from a truncated qcow2.
		emit(fmt.Sprintf("golden image %s on KVM host is %s, expected %s - re-uploading", name, humanSize(staged), humanSize(size)))
	}

	emit(fmt.Sprintf("uploading golden image %s (%s) to KVM host %s - this happens once per image; every later cluster clones it with no upload", name, humanSize(size), h.Addr))
	start := time.Now()
	if err := h.upload(ctx, pool, name, localPath); err != nil {
		return err
	}
	elapsed := time.Since(start)
	emit(fmt.Sprintf("staged golden image %s in %s (%.1f MB/s)", name, elapsed.Round(time.Second), float64(size)/(1<<20)/elapsed.Seconds()))
	return nil
}

// stagedSize returns the size of the named volume's backing file in the pool, or -1 if it is absent.
func (h *Host) stagedSize(ctx context.Context, pool, name string) (int64, error) {
	script := `set -e
dir=$(virsh -c qemu:///system pool-dumpxml "$1" | sed -n 's:.*<path>\(.*\)</path>.*:\1:p' | head -1)
[ -n "$dir" ] || { echo "pool $1 has no target path (not a dir-backed pool?)" >&2; exit 1; }
f="$dir/$2"
if [ -f "$f" ]; then stat -c %s "$f"; else echo missing; fi`
	out, err := h.runSSH(ctx, nil, script, pool, name)
	if err != nil {
		return 0, fmt.Errorf("kvmhost: inspect staged image %q on %s: %w", name, h.Addr, err)
	}
	out = strings.TrimSpace(out)
	if out == "missing" {
		return -1, nil
	}
	var n int64
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0, fmt.Errorf("kvmhost: unexpected size %q for staged image %q", out, name)
	}
	return n, nil
}

// upload streams the local file into the pool's directory and refreshes the pool so libvirt picks
// the new volume up. It writes to a .part file and renames, so a killed transfer can never leave a
// truncated file that looks like a valid staged image to stagedSize.
func (h *Host) upload(ctx context.Context, pool, name, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// $3 is a per-upload suffix: two clusters created at once both want the same image, and sharing
	// one .part file would have them interleave into a corrupt qcow2. Each writes its own temp and
	// renames; rename is atomic, so the loser simply overwrites with identical bytes and no reader
	// ever sees a partial file.
	script := `set -e
dir=$(virsh -c qemu:///system pool-dumpxml "$1" | sed -n 's:.*<path>\(.*\)</path>.*:\1:p' | head -1)
[ -n "$dir" ] || { echo "pool $1 has no target path" >&2; exit 1; }
part="$dir/.$2.$3.part"
trap 'rm -f "$part"' EXIT
cat > "$part"
mv "$part" "$dir/$2"
trap - EXIT
virsh -c qemu:///system pool-refresh "$1" >/dev/null`
	if _, err := h.runSSH(ctx, f, script, pool, name, uploadID()); err != nil {
		return fmt.Errorf("kvmhost: upload golden image %q to %s: %w", name, h.Addr, err)
	}
	return nil
}

// uploadID is a short random tag distinguishing concurrent uploads of the same image.
func uploadID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// runSSH runs a /bin/sh script on the KVM host with positional args, optionally piping stdin in as
// the payload (the image bytes).
//
// The whole remote command is passed as ONE argv element and shell-quoted by us. ssh does not quote
// what it forwards - it joins its arguments with spaces and hands the result to the remote shell -
// so a script passed as several arguments would be word-split on the far side. Note stdin is
// reserved for data, which is why the script rides on argv rather than being fed to `sh -s`.
func (h *Host) runSSH(ctx context.Context, stdin *os.File, script string, args ...string) (string, error) {
	remote := "/bin/sh -c " + shellQuote(script) + " sh" // $0=sh, then the positional args
	for _, a := range args {
		remote += " " + shellQuote(a)
	}
	cmd := exec.CommandContext(ctx, h.SSHBin, append(h.sshOpts(), remote)...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}

// shellQuote wraps s in single quotes for the remote /bin/sh, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func humanSize(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
