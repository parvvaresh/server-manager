// Command sshm is a tiny terminal manager for your SSH servers.
//
//   - keep servers (name / host / port / user / password / key / tags)
//   - search them as you type
//   - open ssh or sftp on the selected one
//   - mark several and open them all in a tiled tmux window
//
// The vault format is the same tab separated file the shell version used, and
// `sshm lock` writes exactly what `openssl enc -aes-256-cbc -pbkdf2` writes,
// so both versions read each other's vaults.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// version is overridden at release time with -ldflags "-X main.version=...".
var version = "0.2.0"

func main() {
	initColors()
	initTTY()
	defer cleanupRunDir()

	// Leave no scratch dir behind when we are killed from outside.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if currentRaw != nil {
			currentRaw.leave()
		}
		cleanupRunDir()
		os.Exit(130)
	}()

	st := NewStore()
	if err := st.EnsureHome(); err != nil {
		die("%v", err)
	}

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "", "ui", "tui":
		load(st)
		NewUI(st).Run()
	case "ls", "list":
		load(st)
		cmdList(st)
	case "add", "new":
		load(st)
		cmdAdd(st, args)
	case "edit":
		load(st)
		cmdEdit(st, args)
	case "rm", "del", "remove":
		load(st)
		cmdRemove(st, args)
	case "ssh", "connect":
		load(st)
		cmdConnect(st, args, "ssh")
	case "sftp":
		load(st)
		cmdConnect(st, args, "sftp")
	case "multi", "split":
		load(st)
		cmdMulti(st, args)
	case "copy-id":
		load(st)
		cmdCopyID(st, args)
	case "lock":
		load(st)
		cmdLock(st)
	case "unlock":
		load(st)
		cmdUnlock(st)
	case "export":
		load(st)
		cmdExport(st, args)
	case "import":
		load(st)
		cmdImport(st, args)
	case "where", "path":
		if st.Encrypted() {
			info("%s (encrypted)", st.Enc)
		} else {
			info("%s", st.Plain)
		}
	case "version", "-v", "--version":
		info("sshm %s", version)
	case "help", "-h", "--help":
		usage()
	default:
		die("unknown command '%s' — try 'sshm help'", cmd)
	}
}

func load(st *Store) {
	if err := st.Load(); err != nil {
		die("%v", err)
	}
}

func save(st *Store) {
	if err := st.Save(); err != nil {
		die("%v", err)
	}
}

// resolve returns the index of a named server, or exits.
func resolve(st *Store, name string) int {
	i := st.Find(name)
	if i < 0 {
		die("no server named '%s' (try 'sshm ls')", name)
	}
	return i
}

func need(args []string, n int, usage string) {
	if len(args) < n {
		die("usage: sshm %s", usage)
	}
}

// ------------------------------------------------------------- commands ----

func cmdList(st *Store) {
	if len(st.List) == 0 {
		info("no servers yet — run 'sshm add'")
		return
	}
	// size the columns to the content, same as the interactive list does
	wname, wtarget := 4, 9
	for _, s := range st.List {
		wname = max(wname, runeLen(s.Name))
		wtarget = max(wtarget, runeLen(s.Label()))
	}
	fmt.Printf("%s  %s  %s  %s%s\n", cFaint,
		pad("NAME", wname), pad("USER@HOST", wtarget), "TAGS", cReset)
	for _, s := range st.List {
		auth := " "
		switch {
		case s.Key != "":
			auth = cBlue + "⚿" + cReset
		case s.Pass != "":
			auth = cYellow + "•" + cReset
		}
		fmt.Printf("%s %s  %s%s%s  %s%s%s\n", auth, pad(s.Name, wname),
			cMuted, pad(s.Label(), wtarget), cReset,
			cFaint, strings.ReplaceAll(s.Tags, ",", " "), cReset)
	}
}

// cmdAdd takes either the full form arguments or nothing, in which case it
// asks field by field:  sshm add NAME USER@HOST[:PORT] [PASSWORD] [TAGS]
func cmdAdd(st *Store, args []string) {
	var s Server
	if len(args) > 0 {
		need(args, 2, "add NAME USER@HOST[:PORT] [PASS] [TAGS]")
		s = Server{Name: clean(args[0]), User: "root", Port: "22"}

		dest := args[1]
		if at := strings.LastIndex(dest, "@"); at >= 0 {
			s.User = dest[:at]
			dest = dest[at+1:]
		}
		if colon := strings.LastIndex(dest, ":"); colon >= 0 {
			s.Host, s.Port = dest[:colon], dest[colon+1:]
		} else {
			s.Host = dest
		}
		if len(args) > 2 {
			s.Pass = clean(args[2])
		}
		if len(args) > 3 {
			s.Tags = clean(args[3])
		}
		if s.Name == "" || s.Host == "" {
			die("usage: sshm add NAME USER@HOST[:PORT] [PASS] [TAGS]")
		}
	} else {
		var ok bool
		s, ok = formServer(nil)
		if !ok {
			return
		}
	}
	if st.Find(s.Name) >= 0 {
		die("a server named '%s' already exists", s.Name)
	}
	st.Put(-1, s)
	save(st)
	info("added %s%s%s → %s", cGreen, s.Name, cReset, s.Label())
}

func cmdEdit(st *Store, args []string) {
	need(args, 1, "edit NAME")
	i := resolve(st, args[0])
	cur := st.List[i]
	s, ok := formServer(&cur)
	if !ok {
		return
	}
	st.Put(i, s)
	save(st)
	info("saved %s", s.Name)
}

func cmdRemove(st *Store, args []string) {
	need(args, 1, "rm NAME")
	i := resolve(st, args[0])
	st.Delete(i)
	save(st)
	info("deleted %s", args[0])
}

func cmdConnect(st *Store, args []string, tool string) {
	need(args, 1, tool+" NAME")
	i := resolve(st, args[0])
	if err := connectNow(st.List[i], tool); err != nil {
		// ssh exiting non-zero is normal enough not to be an sshm failure.
		warn("%v", err)
	}
}

func cmdMulti(st *Store, args []string) {
	need(args, 1, "multi NAME NAME ...")
	idx := make([]int, 0, len(args))
	for _, name := range args {
		idx = append(idx, resolve(st, name))
	}
	if err := multiConnect("ssh", idx, st.List); err != nil {
		die("%v", err)
	}
}

func cmdCopyID(st *Store, args []string) {
	need(args, 1, "copy-id NAME [KEYFILE]")
	i := resolve(st, args[0])
	key := ""
	if len(args) > 1 {
		key = args[1]
	}
	if err := copyID(st.List[i], key); err != nil {
		die("%v", err)
	}
}

func cmdLock(st *Store) {
	if st.Encrypted() {
		die("the vault is already encrypted")
	}
	p1, _ := readSecret("New master password: ")
	p2, _ := readSecret("Repeat: ")
	if p1 == "" {
		die("empty password")
	}
	if p1 != p2 {
		die("passwords do not match")
	}
	blob, err := encryptVault(st.TSV(true), p1)
	if err != nil {
		die("encryption failed: %v", err)
	}
	if err := writeFileAtomic(st.Enc, blob); err != nil {
		die("%v", err)
	}
	st.Master = p1
	os.Remove(st.Plain)
	info("vault encrypted → %s", st.Enc)
}

func cmdUnlock(st *Store) {
	if !st.Encrypted() {
		die("the vault is not encrypted")
	}
	if err := writeFileAtomic(st.Plain, st.TSV(true)); err != nil {
		die("%v", err)
	}
	os.Remove(st.Enc)
	st.Master = ""
	info("vault decrypted → %s (chmod 600)", st.Plain)
}

func cmdExport(st *Store, args []string) {
	withPasswords := len(args) > 0 && args[0] == "--with-passwords"
	os.Stdout.Write(st.TSV(withPasswords))
}

func cmdImport(st *Store, args []string) {
	need(args, 1, "import FILE.tsv")
	data, err := os.ReadFile(args[0])
	if err != nil {
		die("usage: sshm import FILE.tsv (%v)", err)
	}
	added := 0
	for _, s := range parseTSV(data) {
		if st.Find(s.Name) >= 0 {
			warn("skipping duplicate '%s'", s.Name)
			continue
		}
		st.Put(-1, s)
		added++
	}
	save(st)
	info("imported %d servers", added)
}

func usage() {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprintf(w, `%ssshm %s%s — terminal SSH/SFTP server manager

  %ssshm%s                      open the interactive UI (search, connect, split)

  %ssshm add%s [NAME USER@HOST[:PORT] [PASS] [TAGS]]
  %ssshm ls%s                   list servers
  %ssshm edit%s NAME            edit a server
  %ssshm rm%s NAME              delete a server
  %ssshm ssh%s NAME             connect with ssh
  %ssshm sftp%s NAME            connect with sftp
  %ssshm multi%s NAME NAME ...  open them all in a tiled tmux window
  %ssshm copy-id%s NAME [KEY]   install your public key on a server
  %ssshm lock%s / %sunlock%s       encrypt / decrypt the vault (AES-256, PBKDF2)
  %ssshm export%s [--with-passwords] > backup.tsv
  %ssshm import%s FILE.tsv
  %ssshm where%s                show the vault path
  %ssshm version%s | %shelp%s

Env: SSHM_HOME (vault dir), SSHM_SSH_OPTS (extra ssh flags)
`,
		cBold, version, cReset,
		cBold, cReset, cBold, cReset, cBold, cReset, cBold, cReset,
		cBold, cReset, cBold, cReset, cBold, cReset, cBold, cReset,
		cBold, cReset, cBold, cReset, cBold, cReset, cBold, cReset,
		cBold, cReset, cBold, cReset, cBold, cReset, cBold, cReset)
}
