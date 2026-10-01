package primitive

import (
	"bytes"
	"fmt"
	"os"
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
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	restore := func() {
		fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
		term.Restore(fd, old)
	}
	defer restore()
	buf := make([]byte, 16)
	for t.view != doneView {
		t.w, t.h, _ = term.GetSize(int(out.Fd()))
		if t.w <= 0 {
			t.w, t.h = 100, 40
		}
		t.draw()
		n, err := in.Read(buf)
		if err != nil {
			return err
		}
		for _, k := range splitKeys(string(buf[:n])) {
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
	var out []string
	for len(in) > 0 {
		if strings.HasPrefix(in, "\x1b[") {
			end := 2
			for end < len(in) && !(in[end] >= 'A' && in[end] <= 'Z' || in[end] == '~') {
				end++
			}
			if end < len(in) {
				end++
			}
			out = append(out, in[:end])
			in = in[end:]
			continue
		}
		_, size := utf8.DecodeRuneInString(in)
		out = append(out, in[:size])
		in = in[size:]
	}
	return out
}

// key handles one key press; it returns true when the person submits.
func (t *tui) key(k string) bool {
	t.notice = ""
	up, down := k == "\x1b[A" || k == "k", k == "\x1b[B" || k == "j"
	left, right := k == "\x1b[D" || k == "h", k == "\x1b[C" || k == "l"
	pgup, pgdn := k == "\x1b[5~", k == "\x1b[6~" || k == " "
	enter := k == "\r" || k == "\n"
	esc := k == "\x1b"
	set := func(c string) {
		if len(t.shown) == 0 {
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
		case up && t.cursor > 0:
			t.cursor--
		case down && t.cursor < len(t.shown)-1:
			t.cursor++
		case (enter || right) && len(t.shown) > 0:
			t.view, t.scroll = cardView, 0
		case k == "a":
			set("accept")
		case k == "d":
			set("deny")
		case k == "e":
			set("eval")
		case k == "A":
			for i := range t.choice {
				t.choice[i] = "accept"
			}
			t.notice = "All marked accept. Press s to review."
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
	switch t.view {
	case listView:
		t.drawList(&body)
		footer = keys(t.s, "↑↓", "move", "enter", "open", "a/d/e", "accept / decline / agent eval", "A", "accept all", "s", "review", "q", "quit")
	case cardView:
		card(&body, t.s, t.cursor+1, len(t.shown), t.shown[t.cursor], t.byID, t.choice[t.cursor])
		footer = keys(t.s, "↑↓", "scroll", "←→", "previous / next", "a/d/e", "choose and go on", "esc", "list", "s", "review")
	case reviewView:
		t.drawReview(&body)
		footer = keys(t.s, "s", "submit", "esc", "back", "q", "quit without saving")
	}
	lines := strings.Split(strings.TrimRight(body.String(), "\n"), "\n")
	room := t.h - 2
	if t.view == cardView {
		if t.scroll > len(lines)-room {
			t.scroll = max(0, len(lines)-room)
		}
		lines = lines[t.scroll:]
	}
	if t.view == listView {
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
	fmt.Fprintln(out, " "+s.accent("▐▛███▜▌")+"  "+s.bold("TAP Discover")+s.dim(fmt.Sprintf("  ·  %s  ·  %s sessions  ·  %s tool calls",
		t.cfg.Clients, count(t.res.Summary.Sessions), count(t.res.Summary.ToolCalls))))
	fmt.Fprintln(out, " "+s.accent("▝▜█████▛▘")+s.dim("  Reusable primitives found in your agent history"))
	var saved, all float64
	turns := 0
	for _, f := range t.shown {
		saved += inputEquivalent(f.Saved)
		turns += f.TurnsSaved
	}
	all = inputEquivalent(t.res.Summary.Tokens)
	fmt.Fprintln(out)
	fmt.Fprintf(out, " %s to review · %s follow-up turns · ≈%s tokens saved (of ≈%s in your history, input-equivalent)\n",
		s.bold(fmt.Sprint(len(t.shown))), count(turns), tokensText(saved), tokensText(all))
	if n := t.hidden["accept"] + t.hidden["deny"] + t.hidden["eval"]; n > 0 {
		fmt.Fprintln(out, " "+s.dim(fmt.Sprintf("%d decided earlier and not shown: %d accepted, %d declined, %d in refinement (tap discover --revisit to change)",
			n, t.hidden["accept"], t.hidden["deny"], t.hidden["eval"])))
	}
	if len(t.shown) == 0 {
		fmt.Fprintln(out, "\n Nothing new to review.")
		return
	}
	tab := table{head: []string{" ", "Primitive", "Runs", "Turns saved", "Est. tokens", "Values traced", "Open", "Choice"},
		widths: []int{1, 44, 5, 11, 11, 13, 4, 8}, right: map[int]bool{2: true, 3: true, 4: true, 5: true, 6: true}}
	for i, f := range t.shown {
		mark := " "
		if i == t.cursor {
			mark = s.accent("▶")
		}
		name := title(f)
		if f.Status == StatusNewSince {
			name += " (new since " + f.Earlier + ")"
		} else if f.Status == StatusReevaluated {
			name += " (re-evaluated)"
		}
		name = clip(name, 44) // one line per primitive; the card has the full name
		if i == t.cursor {
			name = s.bold(name)
		}
		tab.rows = append(tab.rows, []string{mark, name, count(f.ExecutionCount), count(f.TurnsSaved), "≈" + tokensText(inputEquivalent(f.Saved)),
			fmt.Sprintf("%d of %d", f.Traced, f.Values), fmt.Sprint(f.OpenQuestions), s.choice(t.choice[i])})
	}
	fmt.Fprintln(out)
	tab.render(out, s)
}

func (t *tui) drawReview(out *bytes.Buffer) {
	s := t.s
	section(out, s, "Review before saving")
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
