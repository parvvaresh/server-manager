package main

// Remote file operations for the dashboard's file manager. Every path is
// passed to the remote shell as a quoted positional argument, never spliced
// into the script itself.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// FileEntry is one row of a directory listing.
type FileEntry struct {
	Name  string  `json:"name"`
	Type  string  `json:"type"` // "dir", "file" or "link"
	IsDir bool    `json:"isDir"`
	Size  int64   `json:"size"`
	MTime float64 `json:"mtime"`
	Mode  string  `json:"mode"`
}

type Listing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// GNU find gives us everything in one pass. BusyBox and the BSDs lack
// -printf, so they fall back to stat in a loop.
const lsScript = `p=${1:-$HOME}
cd -- "$p" 2>/dev/null || { echo "cannot open $p" >&2; exit 3; }
pwd
if find . -maxdepth 0 -printf '' >/dev/null 2>&1; then
  echo F
  find . -mindepth 1 -maxdepth 1 -printf '%y%Y\t%s\t%T@\t%m\t%f\0' 2>/dev/null
else
  echo S
  if stat -c %n . >/dev/null 2>&1; then gnu=1; else gnu=; fi
  for f in .[!.]* ..?* *; do
    [ -e "$f" ] || [ -L "$f" ] || continue
    if [ -d "$f" ]; then printf 'd|'; else printf 'f|'; fi
    if [ -n "$gnu" ]; then
      stat -c '%F|%s|%Y|%a|%n' -- "$f" 2>/dev/null | tr -d '\n'
    else
      stat -f '%HT|%z|%m|%Lp|%N' -- "$f" 2>/dev/null | tr -d '\n'
    fi
    printf '\0'
  done
fi
exit 0
`

func listDir(ctx context.Context, s Server, dir string) (*Listing, error) {
	out, err := remoteOutput(ctx, s, lsScript, dir)
	if err != nil {
		return nil, err
	}
	pwd, rest, _ := bytes.Cut(out, []byte("\n"))
	mode, rest, _ := bytes.Cut(rest, []byte("\n"))
	l := &Listing{Path: string(pwd), Entries: []FileEntry{}}
	for _, rec := range bytes.Split(rest, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		var e FileEntry
		var ok bool
		if string(mode) == "F" {
			e, ok = parseFindEntry(string(rec))
		} else {
			e, ok = parseStatEntry(string(rec))
		}
		if ok && e.Name != "." && e.Name != ".." {
			l.Entries = append(l.Entries, e)
		}
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return l, nil
}

func parseFindEntry(rec string) (FileEntry, bool) {
	f := strings.SplitN(rec, "\t", 5)
	if len(f) != 5 || len(f[0]) < 2 {
		return FileEntry{}, false
	}
	e := FileEntry{Name: f[4], Mode: f[3]}
	e.Size, _ = strconv.ParseInt(f[1], 10, 64)
	e.MTime, _ = strconv.ParseFloat(f[2], 64)
	self, target := f[0][0], f[0][1]
	e.Type = typeName(self)
	e.IsDir = target == 'd'
	return e, true
}

func parseStatEntry(rec string) (FileEntry, bool) {
	f := strings.SplitN(rec, "|", 6)
	if len(f) != 6 {
		return FileEntry{}, false
	}
	e := FileEntry{Name: f[5], Mode: f[4], IsDir: f[0] == "d"}
	e.Size, _ = strconv.ParseInt(f[2], 10, 64)
	e.MTime, _ = strconv.ParseFloat(f[3], 64)
	switch kind := strings.ToLower(f[1]); {
	case strings.Contains(kind, "link"):
		e.Type = "link"
	case strings.Contains(kind, "directory"):
		e.Type = "dir"
	default:
		e.Type = "file"
	}
	return e, true
}

func typeName(c byte) string {
	switch c {
	case 'd':
		return "dir"
	case 'l':
		return "link"
	}
	return "file"
}

// remotePath validates a path coming from the browser: it must be absolute,
// and is cleaned so "a/../b" cannot be used to dodge the checks below.
func remotePath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", errors.New("an absolute path is required")
	}
	return path.Clean(p), nil
}

// fileName validates a single path component, such as an uploaded file name.
func fileName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
		return "", errors.New("invalid file name")
	}
	return n, nil
}

func makeDir(ctx context.Context, s Server, p string) error {
	_, err := remoteOutput(ctx, s, `mkdir -p -- "$1"`, p)
	return err
}

func renamePath(ctx context.Context, s Server, from, to string) error {
	_, err := remoteOutput(ctx, s,
		`[ -e "$2" ] || [ -L "$2" ] && { echo "$2 already exists" >&2; exit 4; }
mv -- "$1" "$2"`, from, to)
	return err
}

func removePath(ctx context.Context, s Server, p string) error {
	if p == "/" || path.Dir(p) == "/" {
		return errors.New("refusing to delete a top level directory")
	}
	_, err := remoteOutput(ctx, s, `rm -rf -- "$1"`, p)
	return err
}

// writeFile streams r into a temporary file next to p, then renames it into
// place, so an interrupted upload never leaves a half written file behind.
func writeFile(ctx context.Context, s Server, p string, r io.Reader) error {
	return runRemote(ctx, s, r, io.Discard,
		`t="$1.sshm-part"
if cat > "$t"; then mv -f -- "$t" "$1"; else rm -f -- "$t"; exit 1; fi`, p)
}

// readFile streams a file, or a directory as a gzipped tarball, into w.
func readFile(ctx context.Context, s Server, p string, asTar bool, w io.Writer) error {
	if asTar {
		return runRemote(ctx, s, nil, w,
			`cd -- "$(dirname -- "$1")" && tar czf - -- "$(basename -- "$1")"`, p)
	}
	return runRemote(ctx, s, nil, w,
		`[ -f "$1" ] || { echo "$1 is not a regular file" >&2; exit 3; }
[ -r "$1" ] || { echo "cannot read $1" >&2; exit 3; }
cat -- "$1"`, p)
}

// maxEditSize is the largest file the browser editor will open.
const maxEditSize = 2 << 20

// readText returns a small file for the editor, refusing large or binary ones.
func readText(ctx context.Context, s Server, p string) (string, error) {
	out, err := remoteOutput(ctx, s,
		`[ -f "$1" ] || { echo "$1 is not a regular file" >&2; exit 3; }
n=$(wc -c < "$1" | tr -d ' ') || exit 3
[ "$n" -le "$2" ] || { echo "file is too large to edit here ($n bytes)" >&2; exit 5; }
cat -- "$1"`, p, strconv.Itoa(maxEditSize))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(out, 0) >= 0 {
		return "", errors.New("this looks like a binary file — download it instead")
	}
	return string(out), nil
}

// cwdMark separates a command's output from the directory it finished in,
// which is how a `cd` in the console carries over to the next command.
const cwdMark = "\x1esshm-cwd:"

// runCommand runs a one-off shell command for the console tab. It returns
// the output and the working directory the command left behind.
func runCommand(ctx context.Context, s Server, dir, command string) ([]byte, string, error) {
	var out limitedBuffer
	out.max = 1 << 20
	err := runRemote(ctx, s, strings.NewReader(""), &out,
		`cd -- "${1:-$HOME}" 2>/dev/null || cd
exec 2>&1
eval "$2"
rc=$?
printf '\036sshm-cwd:%s' "$PWD"
exit $rc`, dir, command)
	b := out.Bytes()
	cwd := ""
	if i := bytes.LastIndex(b, []byte(cwdMark)); i >= 0 {
		cwd = string(b[i+len(cwdMark):])
		b = b[:i]
	}
	return b, cwd, err
}
