package main

// `sshm exec`: run one command on several servers at once.
//
//	sshm exec web-1 web-2 -- uptime
//	sshm exec tag:prod -- df -h /
//	sshm exec 'db-*' -g -- systemctl is-active postgresql
//
// Servers run in parallel, each over its own multiplexed connection, with the
// same credentials `sshm ssh` uses.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type execResult struct {
	Server Server
	Out    []byte
	Code   int    // exit status of the command
	Err    string // set when the server could not be reached at all
	Took   time.Duration
}

func (r execResult) ok() bool { return r.Err == "" && r.Code == 0 }

// resolveTargets turns command line targets into servers, in the order given
// and without repeats. A target is a server name, a shell pattern over names
// ('web-*'), tag:NAME for every server with that tag, or "all".
func resolveTargets(list []Server, targets []string) ([]Server, error) {
	var out []Server
	seen := map[string]bool{}
	add := func(s Server) {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s)
		}
	}
	for _, t := range targets {
		matched := false
		switch {
		case t == "all" || t == "@all":
			for _, s := range list {
				add(s)
			}
			matched = len(list) > 0
		case strings.HasPrefix(t, "tag:"):
			tag := strings.TrimPrefix(t, "tag:")
			for _, s := range list {
				if hasTag(s, tag) {
					add(s)
					matched = true
				}
			}
		case strings.ContainsAny(t, "*?["):
			if _, err := path.Match(t, ""); err != nil {
				return nil, fmt.Errorf("bad pattern '%s'", t)
			}
			for _, s := range list {
				if ok, _ := path.Match(t, s.Name); ok {
					add(s)
					matched = true
				}
			}
		default:
			for _, s := range list {
				if s.Name == t {
					add(s)
					matched = true
				}
			}
		}
		if !matched {
			return nil, fmt.Errorf("'%s' matches no server (try 'sshm ls')", t)
		}
	}
	return out, nil
}

func hasTag(s Server, tag string) bool {
	for _, t := range strings.FieldsFunc(s.Tags, func(r rune) bool { return r == ',' || r == ' ' }) {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// runOnMany runs command on every server, at most parallel at a time, and
// calls done (never concurrently) as each one finishes.
func runOnMany(ctx context.Context, servers []Server, command string, parallel int, timeout time.Duration, done func(execResult)) {
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		sem <- struct{}{}
		go func(s Server) {
			defer wg.Done()
			defer func() { <-sem }()
			r := execOne(ctx, s, command, timeout)
			mu.Lock()
			done(r)
			mu.Unlock()
		}(s)
	}
	wg.Wait()
}

func execOne(ctx context.Context, s Server, command string, timeout time.Duration) execResult {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	out, _, err := runCommand(ctx, s, "", command)
	r := execResult{Server: s, Out: out, Took: time.Since(start)}
	var re *RemoteError
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		r.Code, r.Err = -1, fmt.Sprintf("timed out after %s", timeout)
	case errors.As(err, &re) && re.Code != 255:
		r.Code = re.Code
	case errors.As(err, &re):
		// 255 is ssh's own failure. A command can exit 255 too, but then
		// it has usually printed something.
		r.Code = 255
		if len(out) == 0 {
			r.Err = re.Error()
		}
	default:
		r.Code, r.Err = -1, err.Error()
	}
	return r
}

// groupResults puts servers that answered exactly the same together, the
// biggest group first and otherwise in the order the servers were given.
func groupResults(results []execResult, order []Server) [][]execResult {
	pos := map[string]int{}
	for i, s := range order {
		pos[s.Name] = i
	}
	sorted := append([]execResult(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool { return pos[sorted[i].Server.Name] < pos[sorted[j].Server.Name] })

	var groups [][]execResult
	index := map[string]int{}
	for _, r := range sorted {
		key := strconv.Itoa(r.Code) + "\x00" + r.Err + "\x00" + string(r.Out)
		if i, ok := index[key]; ok {
			groups[i] = append(groups[i], r)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, []execResult{r})
	}
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i]) > len(groups[j]) })
	return groups
}

const execUsage = "exec [-g] [-l] [-j N] [-t SECONDS] TARGET... -- COMMAND"

func cmdExec(st *Store, args []string) {
	mode, parallel, timeout := "blocks", 10, time.Duration(0)
	// Everything after "--" is the command. Without "--" there is one
	// target, and everything after it is the command: sshm exec web-1 uptime
	var targets, command []string
	head, sep := args, -1
	for i, a := range args {
		if a == "--" {
			sep = i
			head, command = args[:i], args[i+1:]
			break
		}
	}
	for i := 0; i < len(head); i++ {
		a := head[i]
		switch {
		case a == "-g" || a == "--group":
			mode = "group"
		case a == "-l" || a == "--lines":
			mode = "lines"
		case a == "-j" || a == "--parallel" || a == "-t" || a == "--timeout":
			if i+1 >= len(head) {
				die("usage: sshm %s", execUsage)
			}
			i++
			n, err := strconv.Atoi(head[i])
			if err != nil || n < 1 {
				die("%s needs a positive number, not '%s'", a, head[i])
			}
			if a == "-j" || a == "--parallel" {
				parallel = n
			} else {
				timeout = time.Duration(n) * time.Second
			}
		case strings.HasPrefix(a, "-"):
			die("unknown option '%s' — usage: sshm %s", a, execUsage)
		default:
			targets = append(targets, a)
			if sep < 0 {
				command = head[i+1:]
				i = len(head)
			}
		}
	}
	if len(targets) == 0 || len(command) == 0 {
		die("usage: sshm %s", execUsage)
	}
	servers, err := resolveTargets(st.List, targets)
	if err != nil {
		die("%v", err)
	}
	cmdline := strings.Join(command, " ")

	var results []execResult
	wname := 0
	for _, s := range servers {
		wname = max(wname, runeLen(s.Name))
	}
	runOnMany(context.Background(), servers, cmdline, parallel, timeout, func(r execResult) {
		results = append(results, r)
		switch mode {
		case "blocks":
			printBlock([]execResult{r})
		case "lines":
			printLines(r, wname)
		}
	})
	if mode == "group" {
		for _, g := range groupResults(results, servers) {
			printBlock(g)
		}
	}

	failed, unreachable := 0, 0
	for _, r := range results {
		switch {
		case r.Err != "":
			unreachable++
		case r.Code != 0:
			failed++
		}
	}
	if len(servers) > 1 {
		parts := []string{fmt.Sprintf("%d ok", len(results)-failed-unreachable)}
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", failed))
		}
		if unreachable > 0 {
			parts = append(parts, fmt.Sprintf("%d unreachable", unreachable))
		}
		fmt.Fprintf(os.Stderr, "%s%s%s\n", cFaint, strings.Join(parts, " · "), cReset)
	}
	cleanupRunDir()
	if failed+unreachable > 0 {
		os.Exit(1)
	}
}

// printBlock prints a header naming the servers, then the output they share.
func printBlock(g []execResult) {
	r := g[0]
	names := make([]string, len(g))
	for i, x := range g {
		names[i] = x.Server.Name
	}
	status := cGreen + "ok" + cReset
	switch {
	case r.Err != "":
		status = cRed + "unreachable" + cReset
	case r.Code != 0:
		status = fmt.Sprintf("%sexit %d%s", cRed, r.Code, cReset)
	}
	who := strings.Join(names, ", ")
	if len(g) > 1 {
		who += fmt.Sprintf(" %s(%d servers)%s", cFaint, len(g), cReset)
	} else {
		who += fmt.Sprintf(" %s%s · %s%s", cFaint, r.Server.Label(), r.Took.Round(10*time.Millisecond), cReset)
	}
	fmt.Printf("%s── %s%s%s  %s\n", cAccent, cBold, who, cReset, status)
	body := r.Out
	if r.Err != "" {
		body = []byte(r.Err + "\n")
	}
	os.Stdout.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
	fmt.Println()
}

// printLines prints every line of output behind the server's name, the way
// pdsh does, which keeps the output easy to grep.
func printLines(r execResult, w int) {
	var lines [][]byte
	if out := bytes.TrimRight(r.Out, "\n"); len(out) > 0 && r.Err == "" {
		lines = bytes.Split(out, []byte("\n"))
	}
	switch {
	case r.Err != "":
		lines = append(lines, []byte("[sshm] "+r.Err))
	case r.Code != 0:
		lines = append(lines, []byte(fmt.Sprintf("[sshm] exit %d", r.Code)))
	}
	color := cAccent
	if !r.ok() {
		color = cRed
	}
	for _, ln := range lines {
		fmt.Printf("%s%s%s %s│%s %s\n", color, pad(r.Server.Name, w), cReset, cFaint, cReset, ln)
	}
}
