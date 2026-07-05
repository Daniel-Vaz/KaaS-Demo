package shell

import (
	"context"
	"strings"
)

// Emulator is an in-process terminal: prompt, local echo, line editing (backspace, Ctrl-C) and
// up/down command history, driven one raw input byte at a time and tolerant of multi-byte escape
// sequences (arrow keys) that span messages. It is the shared engine of both fake backends - the
// cluster shell (fake.go) and node SSH (internal/nodessh/fake.go) - so the escape-sequence parser
// and history logic live in exactly one place.
//
// It knows nothing about what commands mean: Render turns a finished command line into output text.
// That is the whole seam - the cluster shell renders synthesized kubectl, node SSH renders a
// synthesized Linux shell, and neither reimplements the terminal.
type Emulator struct {
	// Term is the browser side of the session.
	Term Conn
	// Banner is printed once when the session opens (already \n-delimited; converted to \r\n).
	Banner string
	// Prompt returns the prompt string, reprinted after every command. May carry ANSI colour.
	Prompt func() string
	// Render maps a submitted command line to its output. clear=true is the `clear` builtin: the
	// emulator emits the screen-clear escape and prints nothing. Output uses \n line endings; the
	// emulator converts them to \r\n for raw-mode xterm (a real PTY does this itself).
	Render func(line string) (out string, clear bool)

	line    []byte
	history []string
	histPos int // index into history during navigation; len(history) == "editing a fresh line"
	esc     int // escape-parse state: 0 none, 1 saw ESC (0x1b), 2 saw ESC '['
}

// Run drives the emulator until the peer closes or ctx is cancelled - a normal session end, so it
// returns nil in that case.
func (e *Emulator) Run(ctx context.Context) error {
	e.histPos = 0
	e.writeString(crlf(e.Banner))
	e.writePrompt()
	for {
		isText, data, err := e.Term.ReadMessage()
		if err != nil {
			return nil
		}
		if isText {
			continue // resize/control: nothing to do for an emulated terminal
		}
		for _, b := range data {
			e.onByte(b)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (e *Emulator) onByte(b byte) {
	switch e.esc {
	case 1:
		if b == '[' {
			e.esc = 2
		} else {
			e.esc = 0
		}
		return
	case 2:
		e.esc = 0
		switch b {
		case 'A':
			e.historyPrev()
		case 'B':
			e.historyNext()
		}
		return
	}

	switch {
	case b == 0x1b: // ESC - start of an escape sequence
		e.esc = 1
	case b == '\r' || b == '\n':
		e.enter()
	case b == 0x7f || b == 0x08: // Backspace / Ctrl-H
		e.backspace()
	case b == 0x03: // Ctrl-C - abandon the current line
		e.writeString("^C\r\n")
		e.line = e.line[:0]
		e.histPos = len(e.history)
		e.writePrompt()
	case b == 0x04: // Ctrl-D - no-op (would EOF a real shell)
	case b >= 0x20 && b < 0x7f: // printable
		e.line = append(e.line, b)
		e.writeBytes([]byte{b}) // local echo
	}
}

func (e *Emulator) enter() {
	line := strings.TrimSpace(string(e.line))
	e.writeString("\r\n")
	if line != "" {
		e.history = append(e.history, line)
		out, clear := e.Render(line)
		if clear {
			e.writeString(clearScreen)
			e.resetLine()
			e.writePrompt()
			return
		}
		if out != "" {
			e.writeString(crlf(out))
			e.writeString("\r\n")
		}
	}
	e.resetLine()
	e.writePrompt()
}

func (e *Emulator) resetLine() {
	e.line = e.line[:0]
	e.histPos = len(e.history)
}

func (e *Emulator) backspace() {
	if len(e.line) == 0 {
		return
	}
	e.line = e.line[:len(e.line)-1]
	e.writeString("\b \b")
}

func (e *Emulator) historyPrev() {
	if e.histPos == 0 {
		return
	}
	e.histPos--
	e.replaceLine(e.history[e.histPos])
}

func (e *Emulator) historyNext() {
	if e.histPos >= len(e.history) {
		return
	}
	e.histPos++
	if e.histPos == len(e.history) {
		e.replaceLine("")
		return
	}
	e.replaceLine(e.history[e.histPos])
}

func (e *Emulator) replaceLine(text string) {
	e.line = append(e.line[:0], text...)
	e.writeString("\r" + clearToEOL)
	e.writePrompt()
	e.writeBytes(e.line)
}

func (e *Emulator) writePrompt()           { e.writeString(e.Prompt()) }
func (e *Emulator) writeString(str string) { e.writeBytes([]byte(str)) }

func (e *Emulator) writeBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	_ = e.Term.WriteBinary(b)
}

// ---- terminal control strings (shared by both fakes) ----

const (
	clearScreen = "\x1b[2J\x1b[3J\x1b[H" // clear screen + scrollback, home cursor
	clearToEOL  = "\x1b[K"
)

// crlf converts bare \n to \r\n so raw-mode xterm renders lines correctly (a real PTY already does).
func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
