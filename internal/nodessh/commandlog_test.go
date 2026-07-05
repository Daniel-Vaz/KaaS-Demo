package nodessh

import (
	"fmt"
	"reflect"
	"testing"
)

// TestCommandRecorder covers the best-effort reconstruction of typed command lines from the raw PTY
// input stream - line submission, in-line editing, control keys, and escape sequences.
func TestCommandRecorder(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single command", "ls -la\r", []string{"ls -la"}},
		{"two commands, CR and LF", "echo hi\recho bye\n", []string{"echo hi", "echo bye"}},
		{"backspace edits the line", "lsX\x7f -la\r", []string{"ls -la"}},
		{"ctrl-c abandons the line", "rm -rf /\x03ls\r", []string{"ls"}},
		{"ctrl-u clears the line", "garbage\x15ls\r", []string{"ls"}},
		{"blank lines are dropped", "  \r\r\tls\r", []string{"ls"}},
		{"arrow-key escape is skipped", "ls\x1b[A\r", []string{"ls"}},
		{"bracketed-paste markers are stripped", "\x1b[200~echo pasted\x1b[201~\r", []string{"echo pasted"}},
		{"unsubmitted trailing line is not a command", "ls\rpartial", []string{"ls"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewCommandRecorder(nil)
			rec.feed([]byte(tc.in))
			got := rec.Commands()
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Commands() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCommandRecorderTruncation: beyond maxCommands the surplus is dropped and the session is flagged.
func TestCommandRecorderTruncation(t *testing.T) {
	rec := NewCommandRecorder(nil)
	for i := 0; i < maxCommands+10; i++ {
		rec.feed([]byte(fmt.Sprintf("cmd%d\r", i)))
	}
	if got := len(rec.Commands()); got != maxCommands {
		t.Fatalf("kept %d commands, want the cap %d", got, maxCommands)
	}
	if !rec.Truncated() {
		t.Fatal("expected Truncated() to be true past the cap")
	}
}

// TestCommandRecorderClipsLongLine: a single very long line is clipped, not stored unbounded.
func TestCommandRecorderClipsLongLine(t *testing.T) {
	rec := NewCommandRecorder(nil)
	long := make([]byte, maxCommandLen+500)
	for i := range long {
		long[i] = 'a'
	}
	rec.feed(long)
	rec.feed([]byte("\r"))
	cmds := rec.Commands()
	if len(cmds) != 1 {
		t.Fatalf("got %d commands, want 1", len(cmds))
	}
	if len(cmds[0]) != maxCommandLen {
		t.Fatalf("command length = %d, want clipped to %d", len(cmds[0]), maxCommandLen)
	}
}
