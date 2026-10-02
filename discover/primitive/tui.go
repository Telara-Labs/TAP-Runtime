package primitive

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Interactive reports a terminal on both ends: the full-screen review needs
// to read keys and redraw in place.
func Interactive(in, out *os.File) bool {
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

type view int

const (
	listView view = iota
	cardView
	reviewView
	doneView
)

// tui is the full-screen review: a list of proposed primitives, a scrolling
// card for each, and a review before anything is written.
type tui struct {
	res    Result
	cfg    MenuConfig
	s      style
	shown  []Family
	hidden map[string]int
	byID   map[string]Primitive
	choice []string
	cursor int
	scroll int
	view   view
	w, h   int
	notice string
	out    *os.File
	// filter narrows the list to primitives whose name contains it; typing
	// is true while the person types it after pressing /.
	filter string
	typing bool
	help   bool
}

// visible lists the primitives the filter keeps, by position in shown.
func (t *tui) visible() []int {
	var out []int
	f := strings.ToLower(t.filter)
	for i, fam := range t.shown {
		if f == "" || strings.Contains(strings.ToLower(title(fam)), f) {
			out = append(out, i)
		}
	}
	return out
}

// step moves the cursor to the next (d=1) or previous (d=-1) visible row.
func (t *tui) step(d int) {
	vis := t.visible()
	for i, v := range vis {
		if v == t.cursor && i+d >= 0 && i+d < len(vis) {
			t.cursor = vis[i+d]
			return
		}
	}
	if len(vis) > 0 && !contains(vis, t.cursor) {
		t.cursor = vis[0]
	}
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// RunTUI runs the full-screen review on the terminal. Choices are held until
// the review is submitted; quitting writes nothing. The terminal is restored
// on every exit path.
func RunTUI(in, out *os.File, res Result, cfg MenuConfig) error {
	shown, hidden := Triage(res, LoadLedger(cfg.StateDir))
	t := &tui{res: res, cfg: cfg, s: style{on: true}, shown: shown, hidden: hidden, byID: map[string]Primitive{},
		choice: make([]string, len(shown)), out: out}
	for _, p := range res.Primitives {
		t.byID[p.ID] = p
	}
	fd := int(in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	fmt.Fprint(out, "\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?25l") // alternate screen, capture mouse, hide cursor
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		fmt.Fprint(out, "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l")
		term.Restore(fd, old)
	}
	defer restore()
	buf := make([]byte, 128)
	pending := ""
	for t.view != doneView {
		t.w, t.h, _ = term.GetSize(int(out.Fd()))
		if t.w <= 0 {
			t.w, t.h = 100, 40
		}
		t.s.width = t.w
		t.draw()
		n, err := in.Read(buf)
		if err != nil {
			return err
		}
		keys, rest := splitInput(pending + string(buf[:n]))
		pending = rest
		for _, k := range keys {
			if t.key(k) {
				restore()
				return t.submit(out)
			}
			if t.view == doneView {
				break
			}
		}
	}
	return nil
}

// splitKeys separates the keys in one read: escape sequences (arrows, page
// keys) and single characters, so fast typing or a paste is not one key.
func splitKeys(in string) []string {
	keys, rest := splitInput(in)
	if rest != "" {
		keys = append(keys, rest)
	}
	return keys
}

// splitInput separates complete key and mouse reports from bytes that may be
// the start of a report split across terminal reads.
func splitInput(in string) ([]string, string) {
	var out []string
	for len(in) > 0 {
		if strings.HasPrefix(in, "\x1b[<") {
			end := strings.IndexAny(in[3:], "Mm")
			if end < 0 {
				return out, in
			}
			end += 4
			out = append(out, in[:end])
			in = in[end:]
			continue
		}
		if strings.HasPrefix(in, "\x1b[") {
			end := 2
			for end < len(in) && !(in[end] >= 'A' && in[end] <= 'Z' || in[end] == '~') {
				end++
			}
			if end == len(in) {
				return out, in
			}
			end++
			out = append(out, in[:end])
			in = in[end:]
			continue
		}
		_, size := utf8.DecodeRuneInString(in)
		out = append(out, in[:size])
		in = in[size:]
	}
	return out, ""
}

// mouseWheelDelta returns -1 for wheel up and +1 for wheel down from an SGR
// mouse report. Other mouse buttons are ignored.
func mouseWheelDelta(k string) (int, bool) {
	if !strings.HasPrefix(k, "\x1b[<") || len(k) < 7 || k[len(k)-1] != 'M' {
		return 0, false
	}
	fields := strings.Split(k[3:len(k)-1], ";")
	if len(fields) != 3 {
		return 0, false
	}
	button, err := strconv.Atoi(fields[0])
	if err != nil || button&64 == 0 {
		return 0, false
	}
	if button&1 == 0 {
		return -1, true
	}
	return 1, true
}

// key handles one key press; it returns true when the person submits.
func (t *tui) key(k string) bool {
	if delta, ok := mouseWheelDelta(k); ok {
		if t.help {
			return false
		}
		switch t.view {
		case listView:
			t.step(delta)
		case cardView:
			t.scroll = max(0, t.scroll+delta)
		}
		return false
	}
	t.notice = ""
	if t.help {
		t.help = false
		return false
	}
	if t.typing {
		switch {
		case k == "\r" || k == "\n":
			t.typing = false
		case k == "\x1b":
			t.typing, t.filter = false, ""
		case k == "\x7f" || k == "\b":
			if r := []rune(t.filter); len(r) > 0 {
				t.filter = string(r[:len(r)-1])
			}
		case len(k) > 0 && k[0] >= ' ' && k[0] != 0x7f && !strings.HasPrefix(k, "\x1b"):
			t.filter += k
		}
		t.step(0)
		return false
	}
	if k == "?" {
		t.help = true
		return false
	}
	up, down := k == "\x1b[A" || k == "k", k == "\x1b[B" || k == "j"
	left, right := k == "\x1b[D" || k == "h", k == "\x1b[C" || k == "l"
	pgup, pgdn := k == "\x1b[5~", k == "\x1b[6~" || k == " "
	enter := k == "\r" || k == "\n"
	esc := k == "\x1b"
	set := func(c string) {
		if len(t.visible()) == 0 {
			return
		}
		if c == "accept" && t.shown[t.cursor].APIMode == "needs_refinement" {
			t.notice = "Install unavailable: this pattern has no runnable API. Press e to prepare a handoff."
			return
		}
		if t.choice[t.cursor] == c {
			t.choice[t.cursor] = "" // pressing it again clears the choice
		} else {
			t.choice[t.cursor] = c
		}
	}
	switch t.view {
	case listView:
		switch {
		case up:
			t.step(-1)
		case down:
			t.step(1)
		case k == "/":
			t.typing = true
		case esc && t.filter != "":
			t.filter = ""
		case (enter || right) && len(t.visible()) > 0:
			t.view, t.scroll = cardView, 0
		case k == "a":
			set("accept")
		case k == "d":
			set("deny")
		case k == "e":
			set("eval")
		case k == "A":
			for i := range t.choice {
				if t.shown[i].APIMode != "needs_refinement" {
					t.choice[i] = "accept"
				}
			}
			t.notice = "Runnable flows marked for installation. Patterns without APIs were skipped."
		case k == "s":
			t.view = reviewView
		case k == "q" || esc || k == "\x03":
			t.view = doneView
		}
	case cardView:
		switch {
		case up && t.scroll > 0:
			t.scroll--
		case down:
			t.scroll++
		case pgup:
			t.scroll = max(0, t.scroll-(t.h-6))
		case pgdn:
			t.scroll += t.h - 6
		case left && t.cursor > 0:
			t.cursor, t.scroll = t.cursor-1, 0
		case right && t.cursor < len(t.shown)-1:
			t.cursor, t.scroll = t.cursor+1, 0
		case k == "a" || k == "d" || k == "e":
			if k == "a" && t.shown[t.cursor].APIMode == "needs_refinement" {
				set("accept")
				break
			}
			set(map[string]string{"a": "accept", "d": "deny", "e": "eval"}[k])
			if t.cursor < len(t.shown)-1 {
				t.cursor, t.scroll = t.cursor+1, 0
			} else {
				t.view = reviewView
			}
		case k == "s":
			t.view = reviewView
		case esc || enter || k == "q" || k == "\x7f":
			t.view = listView
		case k == "\x03":
			t.view = doneView
		}
	case reviewView:
		switch {
		case k == "s" || enter:
			return true
		case esc || k == "b" || k == "\x7f":
			t.view = listView
		case k == "q" || k == "\x03":
			t.view = doneView
		}
	}
	return false
}

// draw renders the current view to fit the terminal and replaces the screen.
func (t *tui) draw() {
	var body bytes.Buffer
	var footer string
	switch {
	case t.help:
		t.drawHelp(&body)
		footer = t.s.dim(" Press any key to close help.")
	case t.view == listView:
		t.drawList(&body)
		actions := []string{"d", "dismiss", "e", "prepare handoff", "s", "review choices", "enter", "open", "↑↓", "move", "/", "search", "?", "help", "q", "quit"}
		if len(t.visible()) > 0 && t.shown[t.cursor].APIMode != "needs_refinement" {
			actions = append([]string{"a", "accept and install"}, actions...)
		}
		footer = keys(t.s, actions...)
		if t.typing {
			footer = " Search: " + t.filter + t.s.accent("▌") + t.s.dim("   enter to keep · esc to clear")
		}
	case t.view == cardView:
		card(&body, t.s, t.cursor+1, len(t.shown), t.shown[t.cursor], t.byID, t.choice[t.cursor], t.res.Summary)
		actions := []string{"d", "dismiss", "e", "prepare handoff", "s", "review choices", "↑↓", "scroll", "←→", "previous / next", "esc", "list"}
		if t.shown[t.cursor].APIMode != "needs_refinement" {
			actions = append([]string{"a", "accept and install"}, actions...)
		}
		footer = keys(t.s, actions...)
	case t.view == reviewView:
		t.drawReview(&body)
		footer = keys(t.s, "s", "submit choices", "esc", "back", "q", "quit without saving")
	}
	lines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
	room := t.h - 2
	if t.view == cardView && !t.help {
		if t.scroll > len(lines)-room {
			t.scroll = max(0, len(lines)-room)
		}
		lines = lines[t.scroll:]
	}
	if t.view == listView && !t.help {
		lines = t.keepCursorVisible(lines, room)
	}
	if len(lines) > room {
		lines = lines[:room]
	}
	var frame strings.Builder
	frame.WriteString("\x1b[H\x1b[2J")
	for _, l := range lines {
		frame.WriteString(fit(l, t.w))
		frame.WriteString("\r\n")
	}
	for i := len(lines); i < room; i++ {
		frame.WriteString("\r\n")
	}
	status := t.notice
	if status == "" {
		n := 0
		for _, c := range t.choice {
			if c != "" {
				n++
			}
		}
		status = fmt.Sprintf("%d of %d decided · nothing is saved until you submit", n, len(t.shown))
	}
	frame.WriteString(fit(t.s.dim(" "+status), t.w) + "\r\n")
	frame.WriteString(fit(footer, t.w))
	fmt.Fprint(t.out, frame.String())
}

// keepCursorVisible scrolls the list so the selected row stays on screen.
func (t *tui) keepCursorVisible(lines []string, room int) []string {
	at := -1
	for i, l := range lines {
		if strings.Contains(l, "▶") {
			at = i
			break
		}
	}
	if at < room-2 || at < 0 {
		return lines
	}
	start := at - room/2
	return lines[start:]
}

// fit cuts a line to the terminal width, keeping color codes intact.
func fit(l string, w int) string {
	if width(l) <= w {
		return l
	}
	var b strings.Builder
	n, esc := 0, false
	for _, r := range l {
		switch {
		case r == '\x1b':
			esc = true
		case esc:
			if r == 'm' {
				esc = false
			}
		default:
			if n >= w-1 {
				b.WriteString("…\x1b[0m")
				return b.String()
			}
			n++
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (t *tui) drawList(out *bytes.Buffer) {
	s := t.s
	header(out, s, t.res, t.cfg.Clients)
	var saved float64
	turns := 0
	patterns := 0
	for _, f := range t.shown {
		if f.APIMode == "needs_refinement" {
			patterns++
			continue
		}
		saved += inputEquivalent(f.Saved)
		turns += f.TurnsSaved
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, " %s exact-flow candidates · %s patterns need refinement · potential %s turns, about %s tokens (estimate)\n",
		s.bold(fmt.Sprint(len(t.shown)-patterns)), count(patterns), count(turns), tokensText(saved))
	if n := t.hidden["accept"] + t.hidden["deny"] + t.hidden["eval"]; n > 0 {
		fmt.Fprintln(out, " "+s.dim(fmt.Sprintf("%d decided earlier and not shown: %d installed, %d dismissed, %d handoffs exported · tap discover --revisit to change",
			n, t.hidden["accept"], t.hidden["deny"], t.hidden["eval"])))
	}
	if len(t.shown) == 0 {
		fmt.Fprintln(out, "\n Nothing new to review.")
		return
	}
	vis := t.visible()
	fams, choices, cur := make([]Family, len(vis)), make([]string, len(vis)), -1
	for i, v := range vis {
		fams[i], choices[i] = t.shown[v], t.choice[v]
		if v == t.cursor {
			cur = i
		}
	}
	fmt.Fprintln(out)
	if t.filter != "" {
		fmt.Fprintf(out, " Showing %d of %d matching %q · esc clears\n", len(vis), len(t.shown), t.filter)
	}
	if len(vis) == 0 {
		fmt.Fprintln(out, " No primitive matches.")
		return
	}
	listTable(out, s, fams, choices, cur)
}

func (t *tui) drawHelp(out *bytes.Buffer) {
	s := t.s
	section(out, s, "Keys")
	table{widths: []int{14, 70}, rows: [][]string{
		{"↑ ↓  or  k j", "move through the list, or scroll a primitive"},
		{"enter  or  →", "open the selected primitive"},
		{"← →", "previous / next primitive, when one is open"},
		{"a", "accept and install a runnable flow; unavailable when the API is undefined"},
		{"d", "dismiss: hide it until something new appears under it"},
		{"e", "select a handoff; submit from Review to write it; no agent runs"},
		{"(again)", "pressing the same choice again clears it"},
		{"A", "mark every runnable flow for installation"},
		{"/", "search by name; esc clears"},
		{"s", "open Review; on Review, submit pending choices"},
		{"esc", "back"},
		{"q", "quit; nothing is saved before you submit"},
	}}.render(out, s)
	section(out, s, "Columns")
	table{widths: []int{14, 70}, rows: [][]string{
		{"Uses", "how many times the agent ran this flow in your history"},
		{"Turns saved", "model round-trips the agent would no longer make"},
		{"Tokens saved", "an estimate, priced as fresh input; cached re-reads count at about a tenth"},
		{"Open", "uncertain value links or decisions in the recorded calls"},
	}}.render(out, s)
}

func (t *tui) drawReview(out *bytes.Buffer) {
	s := t.s
	section(out, s, "Review before saving")
	fmt.Fprintln(out, "  Nothing has been saved. Submit to install accepted flows, write handoffs, and hide dismissed patterns.")
	tab := table{head: []string{"#", "Choice", "Primitive"}, widths: []int{3, 7, 70}, right: map[int]bool{0: true}}
	n := 0
	for i, f := range t.shown {
		if t.choice[i] != "" {
			n++
			tab.rows = append(tab.rows, []string{fmt.Sprint(i + 1), s.choice(t.choice[i]), title(f)})
		}
	}
	if n == 0 {
		fmt.Fprintln(out, "  No choices made. Press esc to go back.")
		return
	}
	tab.render(out, s)
	fmt.Fprintf(out, "  %d of %d undecided (left as they are).\n", len(t.shown)-n, len(t.shown))
}

func (t *tui) submit(out *os.File) error {
	byID := t.byID
	return submit(out, style{on: true}, t.shown, t.choice, byID, t.cfg)
}
