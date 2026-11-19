package main

// Remote execution for the web dashboard.
//
// Everything still goes through the system ssh, so ~/.ssh/config, agents,
// keys and stored passwords behave exactly as they do for `sshm ssh`. Each
// server gets one multiplexed master connection (ControlMaster), which keeps
// the dashboard's frequent polls down to a few milliseconds of overhead and
// means a stored password is only sent once.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ctlMu   sync.Mutex
	ctlDir  string
	ctlUsed = map[string]Server{} // control socket -> server it belongs to
)

// controlPath returns the multiplexing socket for a server. It lives in a
// short /tmp directory because unix socket paths are capped at ~104 bytes and
// the macOS temp dir alone eats most of that.
func controlPath(s Server) (string, error) {
	ctlMu.Lock()
	defer ctlMu.Unlock()
	if ctlDir == "" {
		d, err := os.MkdirTemp("/tmp", "sshm-")
		if err != nil {
			if d, err = rundir(); err != nil {
				return "", err
			}
		}
		_ = os.Chmod(d, 0o700)
		ctlDir = d
	}
	sum := sha256.Sum256([]byte(s.Name + "\x00" + s.Target() + "\x00" + s.Port))
	p := filepath.Join(ctlDir, hex.EncodeToString(sum[:6]))
	ctlUsed[p] = s
	return p, nil
}

// stopMasters closes every master connection we opened and removes their
// directory. It is safe to call more than once.
func stopMasters() {
	ctlMu.Lock()
	defer ctlMu.Unlock()
	for p, s := range ctlUsed {
		args := []string{"-o", "ControlPath=" + p, "-O", "exit", s.Target()}
		cmd := exec.Command("ssh", args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = cmd.Run()
	}
	ctlUsed = map[string]Server{}
	if ctlDir != "" && ctlDir != runDir {
		os.RemoveAll(ctlDir)
	}
	ctlDir = ""
}

// remoteSSHArgs is the ssh command line for a non-interactive command. Extra
// options from SSHM_SSH_OPTS come first, because for ssh the first value of
// an option wins — so a user can override any default chosen here.
func remoteSSHArgs(s Server, ctl string) []string {
	port := s.Port
	if port == "" {
		port = "22"
	}
	args := []string{"ssh"}
	if extra := strings.Fields(os.Getenv("SSHM_SSH_OPTS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	args = append(args, "-p", port, "-T",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+ctl,
		"-o", "ControlPersist=600",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		// Nobody is there to answer a host key prompt; trust on first use,
		// but still refuse a key that changed.
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR",
	)
	if s.Key != "" {
		args = append(args, "-i", expandPath(s.Key), "-o", "IdentitiesOnly=yes")
	}
	if usesPassword(s) {
		args = append(args,
			"-o", "PreferredAuthentications=password,keyboard-interactive",
			"-o", "PubkeyAuthentication=no",
			"-o", "NumberOfPasswordPrompts=1")
	} else {
		args = append(args, "-o", "BatchMode=yes")
	}
	return append(args, s.Target())
}

// RemoteError is a failed remote command: its exit status and what it said.
type RemoteError struct {
	Code int
	Msg  string
}

func (e *RemoteError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("remote command failed (exit %d)", e.Code)
}

// remoteScript builds the command line the remote login shell receives: our
// POSIX script handed to sh, with every argument single-quoted. Going through
// `sh -c` keeps it working when the login shell is fish, csh or anything else.
func remoteScript(script string, args ...string) string {
	var b strings.Builder
	b.WriteString("sh -c ")
	b.WriteString(shQuote(script))
	b.WriteString(" sshm")
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(shQuote(a))
	}
	return b.String()
}

// runRemote runs a POSIX sh script on a server. stdin may be nil; stdout
// receives the script's output.
func runRemote(ctx context.Context, s Server, stdin io.Reader, stdout io.Writer, script string, args ...string) error {
	ctl, err := controlPath(s)
	if err != nil {
		return err
	}
	argv := append(remoteSSHArgs(s, ctl), remoteScript(script, args...))
	name, argv, env, err := passwordEnv(s, argv)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var errb limitedBuffer
	errb.max = 16 << 10
	cmd.Stderr = &errb
	// A session of its own means ssh has no terminal to fall back to, so it
	// can never stop and wait on a prompt nobody will see.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = 3 * time.Second

	err = cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	re := &RemoteError{Code: -1, Msg: strings.TrimSpace(errb.String())}
	if ee, ok := err.(*exec.ExitError); ok {
		re.Code = ee.ExitCode()
	} else if re.Msg == "" {
		re.Msg = err.Error()
	}
	if re.Code == 255 && re.Msg == "" {
		re.Msg = "cannot connect to " + s.Label()
	}
	return re
}

// remoteOutput runs a script and returns everything it printed.
func remoteOutput(ctx context.Context, s Server, script string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := runRemote(ctx, s, nil, &out, script, args...)
	return out.Bytes(), err
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); room > 0 {
		if len(p) > room {
			l.Buffer.Write(p[:room])
		} else {
			l.Buffer.Write(p)
		}
	}
	return len(p), nil
}
