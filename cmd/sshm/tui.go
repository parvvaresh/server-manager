package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// UI is the interactive server picker.
type UI struct {
	st     *Store
	query  string
	cur    int          // cursor position inside filt
	top    int          // first visible row of filt
	filt   []int        // indices into st.List currently shown
	mark   map[int]bool // marked servers, by index into st.List
	status string
	raw    *rawMode
}

func NewUI(st *Store) *UI {
	return &UI{st: st, mark: map[int]bool{}}
}

// ------------------------------------------------------------- filtering ---

func (u *UI) matches(s Server) bool {
	hay := strings.ToLower(strings.Join([]string{s.Name, s.User, s.Host, s.Port, s.Tags, s.Note}, " "))
	for _, term := range strings.Fields(strings.ToLower(u.query)) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}

func (u *UI) refilter() {
	u.filt = u.filt[:0]
	for i, s := range u.st.List {
		if u.query == "" || u.matches(s) {
			u.filt = append(u.filt, i)
		}
	}
	if u.cur >= len(u.filt) {
		u.cur = len(u.filt) - 1
	}
	if u.cur < 0 {
		u.cur = 0
	}
}

// selected is the index into st.List under the cursor, or -1.
func (u *UI) selected() int {
	if u.cur < 0 || u.cur >= len(u.filt) {
		return -1
	}
	return u.filt[u.cur]
}

func (u *UI) markedIndices() []int {
	var out []int
	for i := range u.st.List {
		if u.mark[i] {
			out = append(out, i)
		}
	}
	return out
}

// targets is what Enter acts on: every marked server, or else the highlighted one.
func (u *UI) targets() []int {
	if m := u.markedIndices(); len(m) > 0 {
		return m
	}
	if i := u.selected(); i >= 0 {
		return []int{i}
	}
	return nil
}

// -------------------------------------------------------------- drawing ----

// pad truncates or pads a plain (uncoloured) string to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > w {
		if w > 1 {
			return string(r[:w-1]) + "…"
		}
		return string(r[:w])
	}
	return s + strings.Repeat(" ", w-len(r))
}

// padBetween pushes l to the left edge and r to the right edge of a w wide line.
func padBetween(l, r string, w int) string {
	lw, rw := len([]rune(l)), len([]rune(r))
	if lw+rw+1 > w {
		return pad(l, w)
	}
	return l + strings.Repeat(" ", w-lw-rw) + r
}

// layout is the geometry of one frame. The gutter is four cells wide — cursor
// bar, mark dot, auth glyph, space — and the three columns are sized to the
// rows actually on screen, so the table hugs its content instead of sprawling.
type layout struct{ cols, rows, listh, name, target, tags int }

func runeLen(s string) int { return len([]rune(s)) }

func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	return min(max(v, lo), hi)
}

func (u *UI) layout() layout {
	cols, rows := termSize()
	if cols < 40 {
		cols = 40
	}
	l := layout{cols: cols, rows: rows}
	if l.listh = rows - 6; l.listh < 3 {
		l.listh = 3
	}

	// start from the header labels, then grow to fit the widest value
	l.name, l.target, l.tags = 4, 9, 4
	for _, idx := range u.filt {
		s := u.st.List[idx]
		l.name = max(l.name, runeLen(s.Name))
		l.target = max(l.target, runeLen(s.Label()))
		l.tags = max(l.tags, runeLen(s.Tags))
	}
	l.name = clamp(l.name, 8, (cols-10)*40/100)
	l.tags = clamp(l.tags, 6, (cols-10)*25/100)
	// the last two cells of every row belong to the scrollbar and its gutter
	l.target = clamp(l.target, 10, cols-10-l.name-l.tags)
	return l
}

// highlight wraps every occurrence of a search term in on…off. It is a no-op
// when lowercasing would shift the byte offsets under us.
func highlight(s string, terms []string, on, off string) string {
	if len(terms) == 0 {
		return s
	}
	low := strings.ToLower(s)
	if len(low) != len(s) {
		return s
	}
	type span struct{ a, b int }
	var spans []span
	for _, t := range terms {
		for start := 0; t != ""; {
			i := strings.Index(low[start:], t)
			if i < 0 {
				break
			}
			spans = append(spans, span{start + i, start + i + len(t)})
			start += i + len(t)
		}
	}
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].a < spans[j].a })

	var b strings.Builder
	last := 0
	for _, sp := range spans {
		if sp.b <= last {
			continue // already inside a span we emitted
		}
		sp.a = max(sp.a, last)
		b.WriteString(s[last:sp.a])
		b.WriteString(on)
		b.WriteString(s[sp.a:sp.b])
		b.WriteString(off)
		last = sp.b
	}
	b.WriteString(s[last:])
	return b.String()
}

func line(b *strings.Builder, s string) { b.WriteString(s + "\r\n") }

func (u *UI) render() {
	l := u.layout()

	// keep the cursor inside the viewport
	if u.cur < u.top {
		u.top = u.cur
	}
	if u.cur >= u.top+l.listh {
		u.top = u.cur - l.listh + 1
	}
	if u.top < 0 {
		u.top = 0
	}

	var b strings.Builder
	b.WriteString("\033[H\033[2J")
	u.drawTitle(&b, l)
	u.drawSearch(&b, l)
	u.drawColumns(&b, l)
	u.drawRows(&b, l)
	u.drawDetail(&b, l)
	u.drawHelp(&b, l)
	fmt.Print(b.String())
}

func (u *UI) drawTitle(b *strings.Builder, l layout) {
	left := " ◆ sshm " + version
	if u.st.Encrypted() {
		left += " ⚿"
	}
	right := fmt.Sprintf("%d/%d servers", len(u.filt), len(u.st.List))
	if n := len(u.markedIndices()); n > 0 {
		right += fmt.Sprintf(" · %d marked", n)
	}
	line(b, cBarBg+cBarFg+cBold+padBetween(left, right+" ", l.cols)+cReset)
}

func (u *UI) drawSearch(b *strings.Builder, l layout) {
	hint := "type to filter"
	if u.query != "" {
		hint = "esc clears"
	}
	head := cAccent + cBold + " ⌕ " + cReset + cBold + u.query + cReset + cRev + " " + cReset
	gap := l.cols - 4 - len([]rune(u.query)) - len([]rune(hint)) - 1
	if gap > 1 {
		head += strings.Repeat(" ", gap) + cFaint + hint + cReset + " "
	}
	line(b, head)
}

func (u *UI) drawColumns(b *strings.Builder, l layout) {
	line(b, cFaint+"    "+pad("NAME", l.name)+"  "+pad("USER@HOST", l.target)+
		"  "+pad("TAGS", l.tags)+cReset)
}

func (u *UI) drawRows(b *strings.Builder, l layout) {
	if len(u.filt) == 0 {
		u.drawEmpty(b, l)
		return
	}
	terms := strings.Fields(strings.ToLower(u.query))
	rowW := 4 + l.name + 2 + l.target + 2 + l.tags
	for row := 0; row < l.listh; row++ {
		i := u.top + row
		if i >= len(u.filt) {
			line(b, u.scrollbar(row, l))
			continue
		}
		idx := u.filt[i]
		s := u.st.List[idx]

		name := pad(s.Name, l.name)
		target := pad(s.Label(), l.target)
		tags := pad(strings.ReplaceAll(s.Tags, ",", " "), l.tags)

		mark, auth := " ", " "
		if u.mark[idx] {
			mark = "●"
		}
		switch {
		case s.Key != "":
			auth = "⚿"
		case s.Pass != "":
			auth = "•"
		}

		// The highlighted row is drawn in a single style so its background
		// stays unbroken — matches are underlined there rather than coloured.
		if i == u.cur {
			body := pad("▌"+mark+auth+" "+name+"  "+target+"  "+tags, l.cols-1)
			line(b, cSelBg+cSelFg+cBold+highlight(body, terms, "\033[4m", "\033[24m")+
				cReset+u.scrollbar(row, l))
			continue
		}
		switch {
		case s.Key != "":
			auth = cBlue + auth + cReset
		case s.Pass != "":
			auth = cYellow + auth + cReset
		}
		line(b, " "+cMark+mark+cReset+auth+" "+
			highlight(name, terms, cAccent+cBold, cReset)+"  "+
			cMuted+highlight(target, terms, cAccent+cBold, cReset+cMuted)+cReset+"  "+
			cFaint+tags+cReset+strings.Repeat(" ", max(0, l.cols-1-rowW))+
			u.scrollbar(row, l))
	}
}

// scrollbar returns the one cell drawn at the right edge of a list row: a
// thumb showing where the viewport sits, or nothing when everything fits.
func (u *UI) scrollbar(row int, l layout) string {
	if len(u.filt) <= l.listh {
		return ""
	}
	thumb := max(1, l.listh*l.listh/len(u.filt))
	start := u.top * l.listh / len(u.filt)
	if row >= start && row < start+thumb {
		return cAccent + "█" + cReset
	}
	return cFaint + "│" + cReset
}

func (u *UI) drawEmpty(b *strings.Builder, l layout) {
	msg := "no servers yet — press ^N to add one"
	if len(u.st.List) > 0 {
		msg = "nothing matches “" + u.query + "”"
	}
	mid := l.listh / 2
	for row := 0; row < l.listh; row++ {
		if row != mid {
			line(b, "")
			continue
		}
		indent := (l.cols - len([]rune(msg))) / 2
		if indent < 0 {
			indent = 0
		}
		line(b, strings.Repeat(" ", indent)+cFaint+msg+cReset)
	}
}

// drawDetail shows the last action, or what the cursor is sitting on.
func (u *UI) drawDetail(b *strings.Builder, l layout) {
	if u.status != "" {
		line(b, cAccent+pad(" ▸ "+u.status, l.cols)+cReset)
		return
	}
	i := u.selected()
	if i < 0 {
		line(b, "")
		return
	}
	s := u.st.List[i]
	det := " " + s.Target() + ":" + s.Port
	switch {
	case s.Key != "":
		det += "   ⚿ " + s.Key
	case s.Pass != "":
		det += "   • password stored"
	default:
		det += "   ⚿ agent / default keys"
	}
	if s.Note != "" {
		det += "   " + s.Note
	}
	line(b, cFaint+pad(det, l.cols)+cReset)
}

var (
	helpTop = [][2]string{{"↑↓", "move"}, {"⏎", "ssh"}, {"^F", "sftp"},
		{"⇥", "mark"}, {"^T", "split"}, {"^A", "all/none"}}
	helpBottom = [][2]string{{"^N", "new"}, {"^E", "edit"}, {"^X", "delete"},
		{"^Y", "password"}, {"^K", "copy-id"}, {"^R", "reload"},
		{"^G", "help"}, {"^Q", "quit"}}
)

func (u *UI) drawHelp(b *strings.Builder, l layout) {
	line(b, helpLine(helpTop, l.cols))
	b.WriteString(helpLine(helpBottom, l.cols))
}

// helpLine lays out "key label · key label …", dropping whatever does not fit.
func helpLine(pairs [][2]string, cols int) string {
	var b strings.Builder
	b.WriteString(" ")
	w := 1
	for i, p := range pairs {
		seg := len([]rune(p[0])) + 1 + len([]rune(p[1]))
		sep := 0
		if i > 0 {
			sep = 3
		}
		if w+sep+seg > cols-1 {
			break
		}
		if i > 0 {
			b.WriteString(cFaint + " · " + cReset)
		}
		b.WriteString(cAccent + cBold + p[0] + cReset + cMuted + " " + p[1] + cReset)
		w += sep + seg
	}
	return b.String()
}

// ------------------------------------------------------------ suspending ---

// suspend drops out of the alt screen, runs fn on the real terminal, then
// comes back. Everything that talks to the user in cooked mode goes through it.
func (u *UI) suspend(pause bool, fn func()) {
	u.raw.leave()
	fmt.Print("\033[2J\033[H")
	fn()
	if pause {
		pauseKey()
	}
	u.raw = enterRaw()
}

// --------------------------------------------------------------- the loop --

func (u *UI) Run() {
	u.refilter()
	u.raw = enterRaw()
	defer func() {
		u.raw.leave()
		fmt.Println()
	}()

	for {
		u.render()
		key, ok := readKey()
		if !ok {
			return
		}
		u.status = ""

		switch key.Name {
		case "RESIZE":
			continue
		case "^Q", "^C":
			return
		case "UP":
			if u.cur > 0 {
				u.cur--
			}
		case "DOWN":
			if u.cur < len(u.filt)-1 {
				u.cur++
			}
		case "PGUP":
			u.cur -= 10
			if u.cur < 0 {
				u.cur = 0
			}
		case "PGDN":
			u.cur += 10
			if u.cur >= len(u.filt) {
				u.cur = len(u.filt) - 1
			}
			if u.cur < 0 {
				u.cur = 0
			}
		case "HOME":
			u.cur = 0
		case "END":
			u.cur = len(u.filt) - 1
			if u.cur < 0 {
				u.cur = 0
			}
		case "BACKSPACE":
			if r := []rune(u.query); len(r) > 0 {
				u.query = string(r[:len(r)-1])
			}
			u.refilter()
		case "^U", "ESC":
			u.query = ""
			u.refilter()
		case "^R":
			u.reload()
		case "^G":
			u.suspend(true, showHelp)
		case "TAB":
			u.toggleMark()
		case "^A":
			u.markAllOrNone()
		case "ENTER", "^F":
			tool := "ssh"
			if key.Name == "^F" {
				tool = "sftp"
			}
			u.connect(tool, u.targets())
		case "^T":
			u.connect("ssh", u.targets())
		case "^N":
			u.addServer()
		case "^E":
			u.editServer()
		case "^X":
			u.deleteServer()
		case "^Y":
			u.copyPassword()
		case "^K":
			u.runCopyID()
		case "":
			if key.Rune >= ' ' {
				u.query += string(key.Rune)
				u.cur, u.top = 0, 0
				u.refilter()
			}
		}
	}
}

func (u *UI) reload() {
	if err := u.st.Load(); err != nil {
		u.status = "reload failed: " + err.Error()
		return
	}
	u.mark = map[int]bool{}
	u.refilter()
	u.status = "reloaded"
}

func (u *UI) toggleMark() {
	idx := u.selected()
	if idx < 0 {
		return
	}
	if u.mark[idx] {
		delete(u.mark, idx)
	} else {
		u.mark[idx] = true
	}
	if u.cur < len(u.filt)-1 {
		u.cur++
	}
}

func (u *UI) markAllOrNone() {
	if len(u.markedIndices()) > 0 {
		u.mark = map[int]bool{}
		u.status = "marks cleared"
		return
	}
	for _, idx := range u.filt {
		u.mark[idx] = true
	}
	u.status = fmt.Sprintf("marked %d servers", len(u.filt))
}

func (u *UI) connect(tool string, idx []int) {
	if len(idx) == 0 {
		return
	}
	u.suspend(true, func() {
		if err := multiConnect(tool, idx, u.st.List); err != nil {
			warn("%v", err)
		}
	})
}

func (u *UI) save() {
	if err := u.st.Save(); err != nil {
		u.status = "save failed: " + err.Error()
	}
}

func (u *UI) addServer() {
	var added string
	u.suspend(true, func() {
		s, ok := formServer(nil)
		if !ok {
			return
		}
		if u.st.Find(s.Name) >= 0 {
			warn("a server named '%s' already exists", s.Name)
			return
		}
		u.st.Put(-1, s)
		u.save()
		added = s.Name
	})
	if added != "" && u.status == "" {
		u.status = "added " + added
	}
	u.refilter()
}

func (u *UI) editServer() {
	idx := u.selected()
	if idx < 0 {
		return
	}
	var saved string
	u.suspend(true, func() {
		cur := u.st.List[idx]
		s, ok := formServer(&cur)
		if !ok {
			return
		}
		u.st.Put(idx, s)
		u.save()
		saved = s.Name
	})
	if saved != "" && u.status == "" {
		u.status = "saved " + saved
	}
	u.refilter()
}

func (u *UI) deleteServer() {
	idx := u.selected()
	if idx < 0 {
		return
	}
	var deleted bool
	u.suspend(false, func() {
		s := u.st.List[idx]
		fmt.Printf("Delete %s (%s)? [y/N] ", s.Name, s.Label())
		if ans := readLine(); strings.HasPrefix(strings.ToLower(ans), "y") {
			u.st.Delete(idx)
			u.save()
			deleted = true
		}
	})
	if deleted {
		u.mark = map[int]bool{}
		if u.status == "" {
			u.status = "deleted"
		}
	}
	u.refilter()
}

func (u *UI) copyPassword() {
	idx := u.selected()
	if idx < 0 {
		return
	}
	s := u.st.List[idx]
	switch {
	case s.Pass == "":
		u.status = "no password stored for " + s.Name
	default:
		if err := copyToClipboard(s.Pass); err != nil {
			u.status = err.Error()
		} else {
			u.status = "password of " + s.Name + " copied to clipboard"
		}
	}
}

func (u *UI) runCopyID() {
	idx := u.selected()
	if idx < 0 {
		return
	}
	u.suspend(true, func() {
		if err := copyID(u.st.List[idx], ""); err != nil {
			warn("%v", err)
		}
	})
}

// ---------------------------------------------------------------- forms ----

// formRead asks for one field. An empty answer keeps the current value.
func formRead(label, current string, secret bool) string {
	switch {
	case current != "" && secret:
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, "(unchanged)")
	case current != "":
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, current)
	default:
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	var ans string
	if secret {
		ans, _ = readSecret("")
	} else {
		ans = readLine()
	}
	if ans == "" {
		return current
	}
	return clean(ans)
}

// formServer asks for every field of a server. cur is nil when adding one.
func formServer(cur *Server) (Server, bool) {
	s := Server{Port: "22", User: "root"}
	if cur != nil {
		s = *cur
		fmt.Printf("%sEdit server%s %s(empty = keep current)%s\n", cBold, cReset, cDim, cReset)
	} else {
		fmt.Printf("%sNew server%s %s(empty = default in brackets)%s\n", cBold, cReset, cDim, cReset)
	}
	s.Name = formRead("Name        ", s.Name, false)
	if s.Name == "" {
		warn("name is required")
		return s, false
	}
	s.Host = formRead("Host / IP   ", s.Host, false)
	if s.Host == "" {
		warn("host is required")
		return s, false
	}
	s.Port = formRead("Port        ", s.Port, false)
	s.User = formRead("Username    ", s.User, false)
	s.Pass = formRead("Password    ", s.Pass, true)
	s.Key = formRead("Key file    ", s.Key, false)
	s.Tags = formRead("Tags        ", s.Tags, false)
	s.Note = formRead("Note        ", s.Note, false)
	if s.Port == "" {
		s.Port = "22"
	}
	return s, true
}

func showHelp() {
	rows := [][2]string{
		{"", "moving around"},
		{"type", "filter the list — space separates AND terms"},
		{"↑ ↓", "move the cursor"},
		{"PgUp PgDn", "move ten rows"},
		{"Home End", "jump to the first / last row"},
		{"^U  Esc", "clear the search box"},
		{"", "connecting"},
		{"⏎", "ssh to the highlighted server"},
		{"", "…or to every marked server, split with tmux"},
		{"^F", "sftp instead of ssh"},
		{"⇥", "mark / unmark a server"},
		{"^T", "open all marked servers in a tiled tmux window"},
		{"^A", "mark all shown / clear all marks"},
		{"^K", "ssh-copy-id — install your key on the server"},
		{"^Y", "copy the password to the clipboard"},
		{"", "the vault"},
		{"^N", "add a new server"},
		{"^E", "edit the highlighted server"},
		{"^X", "delete the highlighted server"},
		{"^R", "reload the vault from disk"},
		{"^Q  ^C", "quit"},
	}

	fmt.Printf("\n  %s%s◆ sshm %s%s — keys%s\n", cBold, cAccent, version, cReset, cReset)
	for _, r := range rows {
		if r[0] == "" && !strings.HasPrefix(r[1], "…") {
			fmt.Printf("\n  %s%s%s\n", cFaint, strings.ToUpper(r[1]), cReset)
			continue
		}
		fmt.Printf("    %s%s%s  %s%s%s\n",
			cAccent+cBold, pad(r[0], 10), cReset, cMuted, r[1], cReset)
	}

	fmt.Printf("\n  %sIN A SPLIT TMUX WINDOW%s\n", cFaint, cReset)
	for _, r := range [][2]string{
		{"prefix :", "setw synchronize-panes on — type once, into every pane"},
		{"prefix z", "zoom one pane full screen"},
		{"prefix d", "detach (the panes keep running)"},
	} {
		fmt.Printf("    %s%s%s  %s%s%s\n", cAccent+cBold, pad(r[0], 10), cReset, cMuted, r[1], cReset)
	}
	fmt.Printf("\n  %sVault: ~/.config/sshm/ (chmod 600) · `sshm lock` encrypts it.%s\n", cFaint, cReset)
}
