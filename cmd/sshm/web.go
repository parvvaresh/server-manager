package main

// `sshm web` — a local dashboard in the browser: pick a server in the
// sidebar, watch its CPU, memory, disks and network, browse its files, upload
// and download, edit small text files and run one-off commands.
//
// The server only listens on the loopback interface and every request must
// carry a random token, handed over once in the URL and then kept in a
// SameSite=Strict cookie. The Host header is checked too, which stops DNS
// rebinding attacks from reaching it through a hostile web page.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webIndex []byte

const tokenCookie = "sshm_token"

type webApp struct {
	st    *Store
	mu    sync.Mutex
	token string
	hosts map[string]bool // Host header values we answer to
}

func cmdWeb(st *Store, args []string) {
	port, open := 7676, true
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--no-open":
			open = false
		case a == "--port" || a == "-p":
			if i+1 >= len(args) {
				die("usage: sshm web [--port N] [--no-open]")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 || n > 65535 {
				die("invalid port '%s'", args[i])
			}
			port = n
		case strings.HasPrefix(a, "--port="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--port="))
			if err != nil || n < 0 || n > 65535 {
				die("invalid port '%s'", a)
			}
			port = n
		default:
			die("usage: sshm web [--port N] [--no-open]")
		}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil && port != 0 {
		// The default port is taken (another sshm, most likely): any port will do.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		die("cannot listen: %v", err)
	}
	port = ln.Addr().(*net.TCPAddr).Port

	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		die("%v", err)
	}
	app := &webApp{
		st:    st,
		token: hex.EncodeToString(tok),
		hosts: map[string]bool{
			fmt.Sprintf("127.0.0.1:%d", port): true,
			fmt.Sprintf("localhost:%d", port): true,
		},
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, app.token)

	info("%s◆ sshm dashboard%s on %s", cBold+cAccent, cReset, url)
	info("%sOnly this machine can reach it. Press Ctrl+C to stop.%s", cFaint, cReset)
	if open {
		openBrowser(url)
	}

	srv := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		die("%v", err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if !have("xdg-open") {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

func (a *webApp) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/servers", a.get(a.handleServers))
	mux.HandleFunc("/api/ping", a.get(a.handlePing))
	mux.HandleFunc("/api/stats", a.get(a.handleStats))
	mux.HandleFunc("/api/ls", a.get(a.handleList))
	mux.HandleFunc("/api/download", a.get(a.handleDownload))
	mux.HandleFunc("/api/read", a.get(a.handleRead))
	mux.HandleFunc("/api/write", a.post(a.handleWrite))
	mux.HandleFunc("/api/upload", a.post(a.handleUpload))
	mux.HandleFunc("/api/mkdir", a.post(a.handleMkdir))
	mux.HandleFunc("/api/rename", a.post(a.handleRename))
	mux.HandleFunc("/api/rm", a.post(a.handleRemove))
	mux.HandleFunc("/api/exec", a.post(a.handleExec))
	return a.guard(mux)
}

// guard enforces the Host check and the token on every request.
func (a *webApp) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" {
			if t := r.URL.Query().Get("t"); t != "" {
				if !a.tokenOK(t) {
					http.Error(w, "invalid token", http.StatusForbidden)
					return
				}
				http.SetCookie(w, &http.Cookie{
					Name: tokenCookie, Value: a.token, Path: "/",
					HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		c, err := r.Cookie(tokenCookie)
		if err != nil || !a.tokenOK(c.Value) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "Open the link printed by `sshm web` in your terminal.\n")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !a.hosts[strings.TrimPrefix(o, "http://")] {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *webApp) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) == 1
}

func (a *webApp) get(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		h(w, r)
	}
}

func (a *webApp) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		h(w, r)
	}
}

// --------------------------------------------------------------- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// remoteErr reports a failed remote call with a status that tells the UI
// whether the server was unreachable or the operation itself failed.
func remoteErr(w http.ResponseWriter, err error) {
	var re *RemoteError
	switch {
	case errors.Is(err, context.Canceled):
		return // the browser went away
	case errors.Is(err, context.DeadlineExceeded):
		jsonErr(w, http.StatusGatewayTimeout, "the server took too long to answer")
	case errors.As(err, &re) && re.Code == 255:
		jsonErr(w, http.StatusBadGateway, re.Error())
	default:
		jsonErr(w, http.StatusUnprocessableEntity, err.Error())
	}
}

// server looks up the server named in the request.
func (a *webApp) server(w http.ResponseWriter, name string) (Server, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := a.st.Find(name); i >= 0 {
		return a.st.List[i], true
	}
	jsonErr(w, http.StatusNotFound, "no server named '"+name+"'")
	return Server{}, false
}

func withTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// body decodes a small JSON request body.
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return false
	}
	return true
}

// -------------------------------------------------------------- handlers ---

func (a *webApp) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
	_, _ = w.Write(webIndex)
}

type serverView struct {
	Name  string   `json:"name"`
	Label string   `json:"label"`
	Host  string   `json:"host"`
	Port  string   `json:"port"`
	User  string   `json:"user"`
	Tags  []string `json:"tags"`
	Note  string   `json:"note"`
	Auth  string   `json:"auth"` // "key", "password" or "agent"
}

func (a *webApp) handleServers(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	// Pick up servers added from the terminal meanwhile. An encrypted vault
	// is already unlocked, so this never prompts.
	if err := a.st.Load(); err != nil {
		a.mu.Unlock()
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]serverView, 0, len(a.st.List))
	for _, s := range a.st.List {
		v := serverView{Name: s.Name, Label: s.Label(), Host: s.Host, Port: s.Port,
			User: s.User, Note: s.Note, Tags: []string{}, Auth: "agent"}
		for _, t := range strings.FieldsFunc(s.Tags, func(r rune) bool { return r == ',' || r == ' ' }) {
			v.Tags = append(v.Tags, t)
		}
		switch {
		case s.Key != "":
			v.Auth = "key"
		case s.Pass != "":
			v.Auth = "password"
		}
		out = append(out, v)
	}
	a.mu.Unlock()
	writeJSON(w, map[string]any{"servers": out, "version": version})
}

// handlePing only opens a TCP connection to the ssh port: cheap enough to
// run for every server in the sidebar, and it needs no credentials.
func (a *webApp) handlePing(w http.ResponseWriter, r *http.Request) {
	s, ok := a.server(w, r.URL.Query().Get("s"))
	if !ok {
		return
	}
	port := s.Port
	if port == "" {
		port = "22"
	}
	start := time.Now()
	d := net.Dialer{Timeout: 4 * time.Second}
	conn, err := d.DialContext(r.Context(), "tcp", net.JoinHostPort(s.Host, port))
	if err != nil {
		writeJSON(w, map[string]any{"up": false, "error": err.Error()})
		return
	}
	conn.Close()
	writeJSON(w, map[string]any{"up": true, "ms": time.Since(start).Milliseconds()})
}

func (a *webApp) handleStats(w http.ResponseWriter, r *http.Request) {
	s, ok := a.server(w, r.URL.Query().Get("s"))
	if !ok {
		return
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	st, err := collectStats(ctx, s)
	if err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, st)
}

func (a *webApp) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := a.server(w, q.Get("s"))
	if !ok {
		return
	}
	dir := q.Get("path")
	if dir != "" {
		var err error
		if dir, err = remotePath(dir); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	l, err := listDir(ctx, s, dir)
	if err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, l)
}

// lazyWriter holds back the download headers until the first byte arrives,
// so a failure before that can still be answered with a proper error.
type lazyWriter struct {
	w       http.ResponseWriter
	headers func()
	started bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.headers()
	}
	return l.w.Write(p)
}

func (a *webApp) handleDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := a.server(w, q.Get("s"))
	if !ok {
		return
	}
	p, err := remotePath(q.Get("path"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	asTar := q.Get("dir") == "1"
	name := path.Base(p)
	if asTar {
		name += ".tar.gz"
	}
	lw := &lazyWriter{w: w, headers: func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
				asciiName(name), urlEscape(name)))
		w.WriteHeader(http.StatusOK)
	}}
	err = readFile(r.Context(), s, p, asTar, lw)
	if err != nil && !lw.started {
		remoteErr(w, err)
	}
}

func (a *webApp) handleRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := a.server(w, q.Get("s"))
	if !ok {
		return
	}
	p, err := remotePath(q.Get("path"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := withTimeout(r, 60*time.Second)
	defer cancel()
	text, err := readText(ctx, s, p)
	if err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"path": p, "content": text})
}

func (a *webApp) handleWrite(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := a.server(w, q.Get("s"))
	if !ok {
		return
	}
	p, err := remotePath(q.Get("path"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := withTimeout(r, 120*time.Second)
	defer cancel()
	if err := writeFile(ctx, s, p, io.LimitReader(r.Body, maxEditSize+1)); err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": p})
}

// handleUpload streams each file of a multipart form straight to the server,
// without buffering it on this machine.
func (a *webApp) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := a.server(w, q.Get("s"))
	if !ok {
		return
	}
	dir, err := remotePath(q.Get("dir"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	var done []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		name, err := fileName(path.Base(strings.ReplaceAll(part.FileName(), "\\", "/")))
		if err != nil {
			part.Close()
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		err = writeFile(r.Context(), s, path.Join(dir, name), part)
		part.Close()
		if err != nil {
			remoteErr(w, err)
			return
		}
		done = append(done, name)
	}
	writeJSON(w, map[string]any{"uploaded": done})
}

type pathReq struct {
	S    string `json:"s"`
	Path string `json:"path"`
	To   string `json:"to"`
	Cmd  string `json:"cmd"`
}

func (a *webApp) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !body(w, r, &req) {
		return
	}
	s, ok := a.server(w, req.S)
	if !ok {
		return
	}
	p, err := remotePath(req.Path)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	if err := makeDir(ctx, s, p); err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": p})
}

func (a *webApp) handleRename(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !body(w, r, &req) {
		return
	}
	s, ok := a.server(w, req.S)
	if !ok {
		return
	}
	from, err1 := remotePath(req.Path)
	to, err2 := remotePath(req.To)
	if err := errors.Join(err1, err2); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := withTimeout(r, 60*time.Second)
	defer cancel()
	if err := renamePath(ctx, s, from, to); err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": to})
}

func (a *webApp) handleRemove(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !body(w, r, &req) {
		return
	}
	s, ok := a.server(w, req.S)
	if !ok {
		return
	}
	p, err := remotePath(req.Path)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := withTimeout(r, 5*time.Minute)
	defer cancel()
	if err := removePath(ctx, s, p); err != nil {
		remoteErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": p})
}

func (a *webApp) handleExec(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !body(w, r, &req) {
		return
	}
	s, ok := a.server(w, req.S)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		jsonErr(w, http.StatusBadRequest, "empty command")
		return
	}
	ctx, cancel := withTimeout(r, 2*time.Minute)
	defer cancel()
	start := time.Now()
	out, cwd, err := runCommand(ctx, s, req.Path, req.Cmd)
	code := 0
	if err != nil {
		var re *RemoteError
		if !errors.As(err, &re) || re.Code == 255 {
			remoteErr(w, err)
			return
		}
		code = re.Code
	}
	writeJSON(w, map[string]any{
		"output": string(out), "code": code, "cwd": cwd, "ms": time.Since(start).Milliseconds(),
	})
}

// asciiName is the fallback filename for browsers that ignore filename*.
func asciiName(n string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, n)
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
