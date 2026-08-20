package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// tty is the terminal we read keys from: os.Stdin whenever that is a
// terminal, /dev/tty otherwise (so `sshm < file` still gets a usable UI).
var (
	tty *os.File = os.Stdin
	in  *bufio.Reader
)

func initTTY() {
	if !isTerminal(os.Stdin) {
		if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
			tty = f
		}
	}
	in = bufio.NewReader(tty)
	signal.Notify(winch, syscall.SIGWINCH)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// stty runs stty against the terminal and returns its output.
func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = tty
	out, err := cmd.Output()
	return string(out), err
}

// ---------------------------------------------------------------- colours --

var (
	cReset, cBold, cDim, cRev           string
	cCyan, cGreen, cYellow, cRed, cBlue string
	cMag                                string
)

// The UI palette. On a 256 colour terminal these are picked to sit calmly on
// both dark and light backgrounds; anywhere else they fall back to the eight
// ANSI colours, and to nothing at all when there is no colour to be had.
var (
	cAccent string // headings, keys, the cursor bar
	cMuted  string // secondary text
	cFaint  string // separators and hints
	cSelBg  string // background of the highlighted row
	cSelFg  string // text of the highlighted row
	cBarBg  string // title bar
	cBarFg  string
	cMark   string // the marked-row dot
)

func initColors() {
	term := os.Getenv("TERM")
	if !isTerminal(os.Stdout) || term == "" || term == "dumb" || os.Getenv("NO_COLOR") != "" {
		return
	}
	cReset, cBold, cDim, cRev = "\033[0m", "\033[1m", "\033[2m", "\033[7m"
	cCyan, cGreen, cYellow = "\033[36m", "\033[32m", "\033[33m"
	cRed, cBlue, cMag = "\033[31m", "\033[34m", "\033[35m"

	if rich := strings.Contains(term, "256color") || os.Getenv("COLORTERM") != ""; rich {
		cAccent = "\033[38;5;75m"
		cMuted = "\033[38;5;250m"
		cFaint = "\033[38;5;242m"
		cSelBg = "\033[48;5;236m"
		cSelFg = "\033[38;5;231m"
		cBarBg = "\033[48;5;24m"
		cBarFg = "\033[38;5;231m"
		cMark = "\033[38;5;215m"
		return
	}
	cAccent, cMuted, cFaint = cCyan, "", cDim
	cSelBg, cSelFg = cRev, ""
	cBarBg, cBarFg = cRev, ""
	cMark = cYellow
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%ssshm:%s %s\n", cRed, cReset, fmt.Sprintf(format, a...))
	cleanupRunDir()
	os.Exit(1)
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%ssshm:%s %s\n", cYellow, cReset, fmt.Sprintf(format, a...))
}

func info(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
}

// ------------------------------------------------------------- raw mode ----

// rawMode keeps the terminal settings we found, so they can be put back.
//
// min 0 / time 1 makes a read give up after ~100ms with nothing. That is how
// a lone Esc is told apart from the start of an escape sequence, and it keeps
// every read synchronous: no background reader ever competes with ssh for the
// terminal.
type rawMode struct{ saved string }

// currentRaw is the terminal state a signal handler has to undo if we are
// killed while the UI is up.
var currentRaw *rawMode

func enterRaw() *rawMode {
	saved, _ := stty("-g")
	_, _ = stty("raw", "-echo", "-ixon", "min", "0", "time", "1")
	fmt.Print("\033[?1049h\033[?25l") // alt screen, hide cursor
	currentRaw = &rawMode{saved: strings.TrimSpace(saved)}
	return currentRaw
}

func (r *rawMode) leave() {
	currentRaw = nil
	fmt.Print("\033[?25h\033[?1049l") // show cursor, back to the normal screen
	if r.saved != "" {
		_, _ = stty(r.saved)
	} else {
		_, _ = stty("sane")
	}
	// Drop anything typed ahead: it was meant for the UI, not for what runs next.
	_, _ = in.Discard(in.Buffered())
}

// --------------------------------------------------------------- size ------

var winch = make(chan os.Signal, 1)

func termSize() (cols, rows int) {
	cols, rows = 80, 24
	out, err := stty("size")
	if err != nil {
		return cols, rows
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return cols, rows
	}
	if r, err := strconv.Atoi(f[0]); err == nil && r > 0 {
		rows = r
	}
	if c, err := strconv.Atoi(f[1]); err == nil && c > 0 {
		cols = c
	}
	return cols, rows
}

func resized() bool {
	select {
	case <-winch:
		return true
	default:
		return false
	}
}

// --------------------------------------------------------------- keys ------

// Key names follow the shell version: "UP", "DOWN", "PGUP", "PGDN", "HOME",
// "END", "ENTER", "BACKSPACE", "TAB", "ESC", "DEL", "RESIZE", "^A".."^Z", or
// an empty name with Rune set for a literal character.
type Key struct {
	Name string
	Rune rune
}

// readByte waits for one byte, and reports a resize as soon as one lands.
// ok is false when the terminal went away.
func readByte() (b byte, resize bool, ok bool) {
	spins, since := 0, time.Now()
	for {
		if resized() {
			return 0, true, true
		}
		c, err := in.ReadByte()
		if err == nil {
			return c, false, true
		}
		if err != io.EOF {
			return 0, false, false
		}
		// io.EOF here is the min 0 / time 1 read timing out. If those come
		// back with no delay at all the terminal is gone, so do not spin.
		spins++
		if spins > 50 {
			if time.Since(since) < 500*time.Millisecond {
				return 0, false, false
			}
			spins, since = 0, time.Now()
		}
	}
}

// readByteSoon waits about 100ms for a byte; used inside escape sequences.
func readByteSoon() (byte, bool) {
	c, err := in.ReadByte()
	if err != nil {
		return 0, false
	}
	return c, true
}

func readKey() (Key, bool) {
	b, resize, ok := readByte()
	if !ok {
		return Key{}, false
	}
	if resize {
		return Key{Name: "RESIZE"}, true
	}
	switch {
	case b == '\r' || b == '\n':
		return Key{Name: "ENTER"}, true
	case b == 0x7f || b == 0x08:
		return Key{Name: "BACKSPACE"}, true
	case b == '\t':
		return Key{Name: "TAB"}, true
	case b == 0x1b:
		return readEscape(), true
	case b < 0x20:
		return Key{Name: fmt.Sprintf("^%c", 'A'+b-1)}, true
	case b < 0x80:
		return Key{Rune: rune(b)}, true
	}
	// UTF-8: pull the continuation bytes so a multi-byte rune stays whole.
	buf := []byte{b}
	for len(buf) < 4 && !utf8.FullRune(buf) {
		c, ok := readByteSoon()
		if !ok {
			break
		}
		buf = append(buf, c)
	}
	r, _ := utf8.DecodeRune(buf)
	if r == utf8.RuneError {
		return Key{Name: "ESC"}, true
	}
	return Key{Rune: r}, true
}

func readEscape() Key {
	b, ok := readByteSoon()
	if !ok {
		return Key{Name: "ESC"} // nothing followed: a bare Esc
	}
	if b != '[' && b != 'O' {
		return Key{Name: "ESC"}
	}
	c, ok := readByteSoon()
	if !ok {
		return Key{Name: "ESC"}
	}
	switch c {
	case 'A':
		return Key{Name: "UP"}
	case 'B':
		return Key{Name: "DOWN"}
	case 'C':
		return Key{Name: "RIGHT"}
	case 'D':
		return Key{Name: "LEFT"}
	case 'H':
		return Key{Name: "HOME"}
	case 'F':
		return Key{Name: "END"}
	}
	if c >= '0' && c <= '9' {
		// A numeric sequence such as Esc [ 5 ~ — read up to the final byte.
		num := string(c)
		for i := 0; i < 4; i++ {
			d, ok := readByteSoon()
			if !ok || d == '~' || d < '0' || d > '9' {
				break
			}
			num += string(d)
		}
		switch num {
		case "1", "7":
			return Key{Name: "HOME"}
		case "3":
			return Key{Name: "DEL"}
		case "4", "8":
			return Key{Name: "END"}
		case "5":
			return Key{Name: "PGUP"}
		case "6":
			return Key{Name: "PGDN"}
		}
	}
	return Key{Name: "ESC"}
}

// ------------------------------------------------------------- prompts -----

// readLine reads one line in cooked mode.
func readLine() string {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// readSecret reads one line without echoing it.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	_, _ = stty("-echo")
	line := readLine()
	_, _ = stty("echo")
	fmt.Fprintln(os.Stderr)
	return line, nil
}

func pauseKey() {
	fmt.Printf("\n%s— press Enter to go back —%s", cDim, cReset)
	readLine()
}
