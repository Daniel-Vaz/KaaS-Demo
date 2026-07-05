package nodessh

import (
	"strings"
	"sync"

	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
)

// Command-capture limits. A session's audit record must stay bounded - a `yes` loop or a paste of a
// script should not write megabytes into one operation row.
const (
	maxCommands   = 500  // commands kept per session; beyond this the list is marked truncated
	maxCommandLen = 1024 // a single command line is clipped to this many bytes
)

// CommandRecorder wraps a shell.Conn and reconstructs, best-effort, the command lines a user typed
// during a node SSH session - the audit detail behind an OpSSH operation. It tees the browser→node
// direction (ReadMessage, the keystrokes) and leaves the node→browser direction untouched, so it is
// transparent to the session.
//
// Reconstruction is deliberately best-effort and its limits are worth stating: it reads the raw PTY
// INPUT stream, so it recovers plainly-typed command lines (the overwhelmingly common case -
// `systemctl status kubelet`, `journalctl -u kubelet`, `df -h`) but NOT anything the remote shell
// resolves for the user: a Tab-completion, a ↑-recalled history entry, or a heredoc/editor session
// all reach the node but leave no faithful trace here (the recalled text arrives as a cursor escape,
// not the command). It also captures what was typed, not what ran - a line abandoned with Ctrl-C is
// dropped, matching the shell. Production would instead record the full PTY transcript (asciinema-
// style) server-side; this is the lightweight, privacy-lean version - command lines only, never
// output - that answers "what did they do in here?" for normal use.
type CommandRecorder struct {
	inner shell.Conn

	mu        sync.Mutex
	line      []byte
	commands  []string
	truncated bool
	esc       int // CSI parser state: 0 none, 1 saw ESC, 2 inside ESC[/ESC O … final byte
}

// NewCommandRecorder wraps term so the session's typed command lines can be read back afterwards.
func NewCommandRecorder(term shell.Conn) *CommandRecorder {
	return &CommandRecorder{inner: term}
}

// ReadMessage delegates to the wrapped conn and feeds binary frames (keystrokes) to the reconstructor.
// Text frames are control messages (resize) - never command input - so they pass through untouched.
func (r *CommandRecorder) ReadMessage() (bool, []byte, error) {
	isText, data, err := r.inner.ReadMessage()
	if err == nil && !isText {
		r.feed(data)
	}
	return isText, data, err
}

func (r *CommandRecorder) WriteBinary(b []byte) error { return r.inner.WriteBinary(b) }
func (r *CommandRecorder) WriteText(b []byte) error   { return r.inner.WriteText(b) }
func (r *CommandRecorder) Close() error               { return r.inner.Close() }

// feed drives the tiny line editor one input byte at a time, mirroring what a cooked terminal does to
// the user's keystrokes so the recovered line matches what they saw on screen.
func (r *CommandRecorder) feed(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range data {
		switch r.esc {
		case 1:
			// After ESC: a CSI/SS3 introducer opens a sequence; anything else ends it (and is dropped).
			if b == '[' || b == 'O' {
				r.esc = 2
			} else {
				r.esc = 0
			}
			continue
		case 2:
			// Inside a sequence: skip parameter/intermediate bytes, end on a final byte (0x40–0x7e).
			if b >= 0x40 && b <= 0x7e {
				r.esc = 0
			}
			continue
		}

		switch {
		case b == 0x1b: // ESC - start of an escape sequence (arrow keys, bracketed paste, …)
			r.esc = 1
		case b == '\r' || b == '\n': // Enter - submit the line
			r.commit()
		case b == 0x7f || b == 0x08: // Backspace / Ctrl-H
			if len(r.line) > 0 {
				r.line = r.line[:len(r.line)-1]
			}
		case b == 0x03 || b == 0x15: // Ctrl-C (abandon) / Ctrl-U (clear line)
			r.line = r.line[:0]
		case b >= 0x20 && b < 0x7f: // printable
			if len(r.line) < maxCommandLen {
				r.line = append(r.line, b)
			}
		}
	}
}

// commit finalizes the current line as a command, if it holds anything non-blank.
func (r *CommandRecorder) commit() {
	cmd := strings.TrimSpace(string(r.line))
	r.line = r.line[:0]
	if cmd == "" {
		return
	}
	if len(r.commands) >= maxCommands {
		r.truncated = true
		return
	}
	r.commands = append(r.commands, cmd)
}

// Commands returns the reconstructed command lines, in order. Safe to call after the session ends.
func (r *CommandRecorder) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.commands))
	copy(out, r.commands)
	return out
}

// Truncated reports whether the session issued more than maxCommands commands (the surplus is dropped).
func (r *CommandRecorder) Truncated() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.truncated
}
