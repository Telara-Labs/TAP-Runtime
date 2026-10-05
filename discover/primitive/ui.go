package primitive

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
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

// style colors text when the output is a terminal; width is the screen's
// columns (0 means the default).
type style struct {
	on    bool
	width int
}

func (s style) cols() int {
	if s.width > 0 {
		return s.width
	}
	return screen
}

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
	case decisionAcceptDesign:
		return s.good("design")
	case "deny":
		return s.bad(c)
	case "eval":
		return s.info("handoff")
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

// table draws rows under a header; right lists right-aligned columns. Cells
// wider than their column wrap onto further lines.
type table struct {
	// oneLine cuts cells to their column instead of wrapping them.
	oneLine bool
	// flex names the column that shrinks so the table fits the screen, as
	// its position counted from 1 (0: none).
	flex   int
	head   []string
	rows   [][]string
	widths []int
	right  map[int]bool
}

func (t table) render(out io.Writer, s style) {
	t.widths = append([]int(nil), t.widths...)
	if c := t.flex - 1; c >= 0 && c < len(t.widths) {
		total := 1
		for _, w := range t.widths {
			total += w + 3
		}
		if over := total - s.cols(); over > 0 {
			t.widths[c] = max(8, t.widths[c]-over)
		}
	}
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
			} else if t.oneLine {
				cols[i] = []string{clip(c, w)}
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

func keys(s style, items ...string) string {
	var out []string
	for i := 0; i+1 < len(items); i += 2 {
		out = append(out, s.accent("["+items[i]+"]")+" "+items[i+1])
	}
	return " Press " + strings.Join(out, "   ")
}

// effectText says what an effect means for the person running it.
func effectText(e string) string {
	if e == "read" {
		return "only reads"
	}
	return "may change data · you are asked before each call"
}

// tokensText rounds to two significant figures: these are estimates.
func tokensText(t float64) string {
	unit, div := "", 1.0
	switch {
	case t >= 1e9:
		unit, div = "B", 1e9
	case t >= 1e6:
		unit, div = "M", 1e6
	case t >= 1e3:
		unit, div = "k", 1e3
	}
	v := t / div
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f%s", math.Round(v/10)*10, unit)
	case v >= 10:
		return fmt.Sprintf("%.0f%s", v, unit)
	}
	return fmt.Sprintf("%.1f%s", v, unit)
}

// inputsText turns argument keys into what a person supplies: a shell
// command's positional arguments and options, a tool's named fields.
func inputsText(keys string) string {
	var named, opts []string
	positional := 0
	seen := map[string]bool{}
	for _, k := range strings.Split(keys, ", ") {
		k = strings.TrimSpace(k)
		switch {
		case k == "":
		case len(k) > 1 && k[0] == 'p' && strings.Trim(k[1:], "0123456789") == "":
			positional++
		case strings.HasPrefix(k, "-"):
			o := strings.TrimRight(strings.TrimSuffix(k, "="), "0123456789")
			for _, alt := range strings.Split(o, "|") {
				alt = strings.TrimRight(strings.TrimSuffix(alt, "="), "0123456789")
				if alt != "" && alt != "-" && !seen[alt] {
					seen[alt] = true
					opts = append(opts, alt)
				}
			}
		default:
			named = append(named, strings.ReplaceAll(k, "|", " or "))
		}
	}
	var parts []string
	if len(named) > 0 {
		parts = append(parts, strings.Join(named, ", "))
	}
	if positional > 0 {
		parts = append(parts, fmt.Sprintf("%d positional argument(s)", positional))
	}
	if len(opts) > 0 {
		sort.Strings(opts)
		parts = append(parts, "options "+strings.Join(opts, " "))
	}
	return strings.Join(parts, " · ")
}
