package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runDir is a 0700 scratch dir holding the askpass helper and the per-pane
// launcher scripts. It is created on demand and removed when sshm exits.
var runDir string

func rundir() (string, error) {
	if runDir != "" {
		return runDir, nil
	}
	d, err := os.MkdirTemp("", "sshm.")
	if err != nil {
		return "", fmt.Errorf("cannot create temp dir: %w", err)
	}
	_ = os.Chmod(d, 0o700)
	runDir = d
	return runDir, nil
}

func cleanupRunDir() {
	if runDir != "" {
		os.RemoveAll(runDir)
		runDir = ""
	}
}

func have(prog string) bool {
	_, err := exec.LookPath(prog)
	return err == nil
}

// expandPath resolves ~ and $VARS in a key path, the way the shell did.
func expandPath(p string) string {
	if p == "" {
		return p
	}
	p = os.ExpandEnv(p)
	if p == "~" {
		return userHome()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(userHome(), p[2:])
	}
	return p
}

// usesPassword reports whether we should feed a stored password to ssh: only
// when there is one and no key file takes precedence.
func usesPassword(s Server) bool {
	return s.Pass != "" && s.Key == ""
}

// buildArgs returns the full ssh/sftp command line for a server.
func buildArgs(s Server, tool string) []string {
	port := s.Port
	if port == "" {
		port = "22"
	}
	var args []string
	if tool == "sftp" {
		args = []string{"sftp", "-o", "Port=" + port}
	} else {
		args = []string{"ssh", "-p", port}
	}
	if s.Key != "" {
		args = append(args, "-i", expandPath(s.Key), "-o", "IdentitiesOnly=yes")
	}
	if usesPassword(s) {
		args = append(args,
			"-o", "PreferredAuthentications=password,keyboard-interactive",
			"-o", "PubkeyAuthentication=no")
	}
	// User extras, e.g. SSHM_SSH_OPTS="-o StrictHostKeyChecking=accept-new"
	if extra := strings.Fields(os.Getenv("SSHM_SSH_OPTS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	return append(args, s.Target())
}

// askpassHelper writes (once) the helper that prints $SSHM_PASS for ssh.
func askpassHelper() (string, error) {
	dir, err := rundir()
	if err != nil {
		return "", err
	}
	helper := filepath.Join(dir, "askpass.sh")
	if _, err := os.Stat(helper); err == nil {
		return helper, nil
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$SSHM_PASS\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		return "", err
	}
	return helper, nil
}

func display() string {
	if d := os.Getenv("DISPLAY"); d != "" {
		return d
	}
	return ":0"
}

// passwordEnv returns the environment that makes ssh pick up a stored
// password, either through sshpass or through the askpass helper.
func passwordEnv(s Server, args []string) (cmd string, argv []string, env []string, err error) {
	if !usesPassword(s) {
		return args[0], args[1:], nil, nil
	}
	if have("sshpass") {
		return "sshpass", append([]string{"-e"}, args...),
			[]string{"SSHPASS=" + s.Pass}, nil
	}
	helper, err := askpassHelper()
	if err != nil {
		return "", nil, nil, err
	}
	return args[0], args[1:], []string{
		"SSHM_PASS=" + s.Pass,
		"SSH_ASKPASS=" + helper,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=" + display(),
	}, nil
}

// run executes a command in the foreground, wired to our terminal.
func run(name string, argv []string, extraEnv []string) error {
	cmd := exec.Command(name, argv...)
	cmd.Stdin = tty
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	return cmd.Run()
}

// connectNow runs ssh or sftp for one server in the foreground.
func connectNow(s Server, tool string) error {
	name, argv, env, err := passwordEnv(s, buildArgs(s, tool))
	if err != nil {
		return err
	}
	return run(name, argv, env)
}

// shQuote quotes a value for embedding in a generated /bin/sh script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// makeLauncher writes a self-contained script that opens one server; tmux
// runs one of these per pane.
func makeLauncher(idx int, s Server, tool string) (string, error) {
	dir, err := rundir()
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, fmt.Sprintf("pane-%d-%s.sh", idx, tool))

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "printf '\\033]0;sshm: %%s\\007' %s\n", shQuote(s.Name))
	if usesPassword(s) {
		helper, err := askpassHelper()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "SSHM_PASS=%s; export SSHM_PASS\n", shQuote(s.Pass))
		fmt.Fprintf(&b, "SSH_ASKPASS=%s; export SSH_ASKPASS\n", shQuote(helper))
		b.WriteString("SSH_ASKPASS_REQUIRE=force; export SSH_ASKPASS_REQUIRE\n")
		fmt.Fprintf(&b, "DISPLAY=%s; export DISPLAY\n", shQuote(display()))
	}
	quoted := make([]string, 0, 8)
	for _, a := range buildArgs(s, tool) {
		quoted = append(quoted, shQuote(a))
	}
	b.WriteString(strings.Join(quoted, " ") + "\n")
	b.WriteString("rc=$?\n")
	fmt.Fprintf(&b, "printf '\\n[sshm] %%s exited (%%s). Press Enter to close this pane.\\n' %s \"$rc\"\n",
		shQuote(s.Name))
	b.WriteString("read _ignore\n")

	if err := os.WriteFile(file, []byte(b.String()), 0o700); err != nil {
		return "", err
	}
	return file, nil
}

// multiConnect opens every server in one tiled tmux window. A single server
// just takes over the terminal, no tmux needed.
func multiConnect(tool string, idx []int, servers []Server) error {
	if len(idx) == 0 {
		return errors.New("nothing selected")
	}
	if len(idx) == 1 {
		return connectNow(servers[idx[0]], tool)
	}
	if !have("tmux") {
		return errors.New("tmux is required for split panes — install it (brew install tmux / apt install tmux)")
	}
	if _, err := rundir(); err != nil {
		return err
	}

	session := fmt.Sprintf("sshm-%d-%d", os.Getpid(), time.Now().Unix())
	for n, i := range idx {
		script, err := makeLauncher(i, servers[i], tool)
		if err != nil {
			return err
		}
		if n == 0 {
			if err := run("tmux", []string{"new-session", "-d", "-s", session, "-n", "sshm", script}, nil); err != nil {
				return err
			}
			continue
		}
		if err := run("tmux", []string{"split-window", "-t", session + ":0", script}, nil); err != nil {
			return err
		}
		_ = run("tmux", []string{"select-layout", "-t", session + ":0", "tiled"}, nil)
	}
	_ = run("tmux", []string{"select-layout", "-t", session + ":0", "tiled"}, nil)
	_ = run("tmux", []string{"set-option", "-t", session, "-q", "mouse", "on"}, nil)

	if os.Getenv("TMUX") != "" {
		return run("tmux", []string{"switch-client", "-t", session}, nil)
	}
	return run("tmux", []string{"attach-session", "-t", session}, nil)
}

// copyID installs the local public key on a server with ssh-copy-id.
func copyID(s Server, key string) error {
	if !have("ssh-copy-id") {
		return errors.New("ssh-copy-id not found")
	}
	port := s.Port
	if port == "" {
		port = "22"
	}
	args := []string{"ssh-copy-id", "-p", port}
	if key != "" {
		args = append(args, "-i", expandPath(key))
	}
	args = append(args, s.Target())

	// Here the password is worth using even when a key file is configured:
	// the whole point is to get the key installed in the first place.
	if s.Pass != "" {
		if have("sshpass") {
			return run("sshpass", append([]string{"-e"}, args...), []string{"SSHPASS=" + s.Pass})
		}
		helper, err := askpassHelper()
		if err != nil {
			return err
		}
		return run(args[0], args[1:], []string{
			"SSHM_PASS=" + s.Pass,
			"SSH_ASKPASS=" + helper,
			"SSH_ASKPASS_REQUIRE=force",
			"DISPLAY=" + display(),
		})
	}
	return run(args[0], args[1:], nil)
}

// copyToClipboard pipes text into whichever clipboard tool is installed.
func copyToClipboard(text string) error {
	candidates := [][]string{
		{"pbcopy"},
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
	}
	for _, c := range candidates {
		if !have(c[0]) {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}
	return errors.New("no clipboard tool found (pbcopy/xclip/wl-copy/xsel)")
}
