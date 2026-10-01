package primitive

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Pricing ratios used only to put token counts on one scale: a cache read
// costs about a tenth of a fresh input token and an output token about five
// times one (Claude's published list prices). The result is an estimate,
// labeled as such wherever it is shown.
const (
	cachedRatio = 0.1
	outputRatio = 5.0
)

// inputEquivalent prices a usage in fresh-input tokens.
func inputEquivalent(u trace.Usage) float64 {
	return u.Fresh + u.Cached*cachedRatio + u.Output*outputRatio
}

func cachedShare(u trace.Usage) float64 {
	if u.Total() == 0 {
		return 0
	}
	return 100 * u.Cached / u.Total()
}

// style colors text when the output is a terminal.
type style struct{ on bool }

func (s style) wrap(code, t string) string {
	if !s.on {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}
func (s style) accent(t string) string { return s.wrap("38;5;209", t) }
func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) good(t string) string   { return s.wrap("32", t) }
func (s style) bad(t string) string    { return s.wrap("31", t) }
func (s style) info(t string) string   { return s.wrap("36", t) }

func (s style) choice(c string) string {
	switch c {
	case "accept":
		return s.good(c)
	case "deny":
		return s.bad(c)
	case "eval":
		return s.info(c)
	}
	return c
}

// width counts what a terminal shows: runes, ignoring color codes.
func width(t string) int {
	n, esc := 0, false
	for _, r := range t {
		switch {
		case r == '\x1b':
			esc = true
		case esc && r == 'm':
			esc = false
		case !esc:
			n++
		}
	}
	return n
}

func pad(t string, w int, right bool) string {
	if d := w - width(t); d > 0 {
		if right {
			return strings.Repeat(" ", d) + t
		}
		return t + strings.Repeat(" ", d)
	}
	return t
}

func clip(t string, w int) string {
	if utf8.RuneCountInString(t) <= w {
		return t
	}
	r := []rune(t)
	return string(r[:w-1]) + "…"
}

// wrapText breaks t into lines of at most w runes at spaces or commas.
func wrapText(t string, w int) []string {
	if utf8.RuneCountInString(t) <= w {
		return []string{t}
	}
	var lines []string
	line := ""
	for _, word := range strings.SplitAfter(t, " ") {
		if utf8.RuneCountInString(line+word) > w && line != "" {
			lines = append(lines, strings.TrimRight(line, " "))
			line = ""
		}
		for utf8.RuneCountInString(word) > w {
			r := []rune(word)
			lines = append(lines, string(r[:w]))
			word = string(r[w:])
		}
		line += word
	}
	if strings.TrimSpace(line) != "" {
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return lines
}

const screen = 100

func banner(out io.Writer, s style, sub string) {
	art := []string{
		"████████╗ █████╗ ██████╗ ",
		"╚══██╔══╝██╔══██╗██╔══██╗",
		"   ██║   ███████║██████╔╝",
		"   ██║   ██╔══██║██╔═══╝ ",
		"   ██║   ██║  ██║██║     ",
		"   ╚═╝   ╚═╝  ╚═╝╚═╝     ",
	}
	side := []string{"", s.bold("Discover"), "Reusable primitives found in your", "agent history, ready to review.", "", s.dim(sub)}
	inner := screen - 4
	fmt.Fprintln(out, s.accent("╭"+strings.Repeat("─", inner+2)+"╮"))
	for i, a := range art {
		line := "  " + s.accent(a) + "   " + side[i]
		fmt.Fprintln(out, s.accent("│")+" "+pad(line, inner, false)+" "+s.accent("│"))
	}
	fmt.Fprintln(out, s.accent("╰"+strings.Repeat("─", inner+2)+"╯"))
}

// table draws rows under a header; right lists right-aligned columns. Cells
// wider than their column wrap onto further lines.
type table struct {
	head   []string
	rows   [][]string
	widths []int
	right  map[int]bool
}

func (t table) render(out io.Writer, s style) {
	sep := func(l, m, r string) string {
		var parts []string
		for _, w := range t.widths {
			parts = append(parts, strings.Repeat("─", w+2))
		}
		return s.dim(l + strings.Join(parts, m) + r)
	}
	row := func(cells []string, bold bool) {
		cols := make([][]string, len(t.widths))
		h := 1
		for i, w := range t.widths {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			if width(c) != utf8.RuneCountInString(c) { // colored: no wrapping
				cols[i] = []string{c}
			} else {
				cols[i] = wrapText(c, w)
			}
			if len(cols[i]) > h {
				h = len(cols[i])
			}
		}
		for l := 0; l < h; l++ {
			var parts []string
			for i, w := range t.widths {
				c := ""
				if l < len(cols[i]) {
					c = cols[i][l]
				}
				c = pad(c, w, t.right[i])
				if bold {
					c = s.bold(c)
				}
				parts = append(parts, " "+c+" ")
			}
			fmt.Fprintln(out, s.dim("│")+strings.Join(parts, s.dim("│"))+s.dim("│"))
		}
	}
	fmt.Fprintln(out, sep("┌", "┬", "┐"))
	if len(t.head) > 0 {
		row(t.head, true)
		fmt.Fprintln(out, sep("├", "┼", "┤"))
	}
	for _, r := range t.rows {
		row(r, false)
	}
	fmt.Fprintln(out, sep("└", "┴", "┘"))
}

func section(out io.Writer, s style, title string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, " "+s.accent("▍")+s.bold(title))
}

func count(n int) string {
	t := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range t {
		if i > 0 && (len(t)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tokenCells describes a usage as total, cached share and estimated
// input-equivalent.
func tokenCells(u trace.Usage) (string, string, string) {
	return tokensText(u.Total()), fmt.Sprintf("%.0f%%", cachedShare(u)), "≈" + tokensText(inputEquivalent(u))
}

func keys(s style, items ...string) string {
	var out []string
	for i := 0; i+1 < len(items); i += 2 {
		out = append(out, s.accent("["+items[i]+"]")+" "+items[i+1])
	}
	return " " + strings.Join(out, "   ")
}
