package main

// The dashboard's terminal: a real login shell on the server, in the browser.
//
// The page talks to /api/term over a WebSocket. On this side ssh runs on a
// pseudo-terminal of its own, so it behaves just as it does in a terminal
// window: full screen programs, colours, job control, Ctrl+C, and a window
// size that follows the browser. ssh joins the server's multiplexed
// connection, so opening a terminal needs no new login.
//
// Binary frames carry raw terminal bytes, both ways. Text frames carry JSON
// control messages: {"t":"resize","cols":N,"rows":N} from the page, and
// {"t":"exit","code":N} or {"t":"error","msg":"..."} from us.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type termMsg struct {
	T    string `json:"t"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Code *int   `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// termSession is one ssh running on a pseudo-terminal.
type termSession struct {
	cmd  *exec.Cmd
	pty  *os.File
	done chan struct{} // closed once ssh has exited
	code int
}

var (
	termMu    sync.Mutex
	termLive  = map[*termSession]bool{}
	termShell = `cd -- "$1" 2>/dev/null || cd; exec "${SHELL:-/bin/sh}" -l`
)

// startTerminal runs ssh for s on a new pseudo-terminal of the given size,
// starting in dir on the server, or in the home directory when dir is empty.
func startTerminal(s Server, dir string, cols, rows int) (*termSession, error) {
	ctl, err := controlPath(s)
	if err != nil {
		return nil, err
	}
	argv := sshArgs(s, ctl, true)
	if dir != "" {
		argv = append(argv, remoteScript(termShell, dir))
	}
	// sshpass would put a terminal of its own between ssh and ours, so a
	// stored password always goes through the askpass helper here. Usually
	// it is not needed at all: the master connection is already logged in.
	env, err := askpassEnv(s)
	if err != nil {
		return nil, err
	}
	master, tty, err := openPTY()
	if err != nil {
		return nil, err
	}
	_ = setWinsize(master, cols, rows)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(append(os.Environ(), "TERM=xterm-256color"), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// A session of its own, with the pseudo-terminal as its controlling
	// terminal: that is what makes ssh see a terminal, follow its size and
	// get SIGHUP when it goes away.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = cmd.Start()
	tty.Close() // ssh has its own copy; ours would keep the terminal from ever closing
	if err != nil {
		master.Close()
		return nil, err
	}
	t := &termSession{cmd: cmd, pty: master, done: make(chan struct{})}
	termMu.Lock()
	termLive[t] = true
	termMu.Unlock()
	go func() {
		err := cmd.Wait()
		t.code = 0
		if ee, ok := err.(*exec.ExitError); ok {
			t.code = ee.ExitCode()
		} else if err != nil {
			t.code = -1
		}
		termMu.Lock()
		delete(termLive, t)
		termMu.Unlock()
		close(t.done)
	}()
	return t, nil
}

// hangup ends the session the way closing a terminal window does, and
// makes sure of it a moment later.
func (t *termSession) hangup() {
	pid := t.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	select {
	case <-t.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// closeTerminals hangs up every terminal still open, e.g. on Ctrl+C.
func closeTerminals() {
	termMu.Lock()
	live := make([]*termSession, 0, len(termLive))
	for t := range termLive {
		live = append(live, t)
	}
	termMu.Unlock()
	for _, t := range live {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGHUP)
	}
}

func clampSize(v string, def, max int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func (a *webApp) handleTerm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a.mu.Lock()
	i := a.st.Find(q.Get("s"))
	var s Server
	if i >= 0 {
		s = a.st.List[i]
	}
	a.mu.Unlock()
	cols := clampSize(q.Get("cols"), 80, 1000)
	rows := clampSize(q.Get("rows"), 24, 500)

	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	send := func(m termMsg) {
		b, _ := json.Marshal(m)
		_ = ws.WriteMessage(wsText, b)
	}
	// Problems are reported over the socket: a failed handshake would only
	// tell the page that something went wrong, not what.
	if i < 0 {
		send(termMsg{T: "error", Msg: "no server named '" + q.Get("s") + "'"})
		return
	}
	dir := q.Get("path")
	if dir != "" {
		if dir, err = remotePath(dir); err != nil {
			send(termMsg{T: "error", Msg: err.Error()})
			return
		}
	}

	// Log in through the usual path first (stored password and all), so
	// the terminal's ssh only has to join the master connection.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = remoteOutput(ctx, s, "exit 0")
	cancel()
	if err != nil {
		var re *RemoteError
		msg := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "the server took too long to answer"
		} else if errors.As(err, &re) && re.Msg == "" {
			msg = "cannot connect to " + s.Label()
		}
		send(termMsg{T: "error", Msg: msg})
		return
	}

	t, err := startTerminal(s, dir, cols, rows)
	if err != nil {
		send(termMsg{T: "error", Msg: "cannot start ssh: " + err.Error()})
		return
	}
	defer t.pty.Close()

	// Browser to terminal. When the page goes away, hang up.
	go func() {
		for {
			op, data, err := ws.ReadMessage()
			if err != nil {
				t.hangup()
				return
			}
			switch op {
			case wsBinary:
				if _, err := t.pty.Write(data); err != nil {
					return
				}
			case wsText:
				var m termMsg
				if json.Unmarshal(data, &m) == nil && m.T == "resize" {
					_ = setWinsize(t.pty, clampSize(strconv.Itoa(m.Cols), 80, 1000),
						clampSize(strconv.Itoa(m.Rows), 24, 500))
				}
			}
		}
	}()

	// Terminal to browser, until ssh is gone and the terminal has drained.
	buf := make([]byte, 32<<10)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			if ws.WriteMessage(wsBinary, buf[:n]) != nil {
				t.hangup()
				break
			}
		}
		if err != nil {
			break
		}
	}
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
		t.hangup()
		<-t.done
	}
	code := t.code
	send(termMsg{T: "exit", Code: &code})
}
