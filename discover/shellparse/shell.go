package shellparse

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Word is one shell word. Quoted records that any part of it was quoted, which
// is how free text (a commit message, a search pattern) is told from a word.
type Word struct {
	Text   string
	Quoted bool
}

// SplitShell breaks a command line into simple commands at &&, ||, ;, | and
// newlines outside quotes, and each simple command into words. Heredoc bodies
// are dropped; the command that reads them is kept. $(...) and backquotes stay
// inside the word they appear in. This is a tokenizer for counting, not a
// shell: a malformed line still yields its best-effort words.
func SplitShell(line string) [][]Word {
	var (
		out     [][]Word
		cur     []Word
		buf     strings.Builder
		quoted  bool
		inWord  bool
		heredoc []string
	)
	endWord := func() {
		if inWord {
			cur = append(cur, Word{Text: buf.String(), Quoted: quoted})
		}
		buf.Reset()
		quoted, inWord = false, false
	}
	endCmd := func() {
		endWord()
		if len(cur) > 0 {
			out = append(out, cur)
		}
		cur = nil
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(rs) && rs[j] != c {
				if rs[j] == '\\' && c != '\'' {
					j++
				}
				j++
			}
			// An unterminated quote runs to the end of the line.
			end := min(j, len(rs))
			buf.WriteString(string(rs[i+1 : end]))
			quoted, inWord = true, true
			i = end
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			depth, j := 0, i+1
			for ; j < len(rs); j++ {
				if rs[j] == '(' {
					depth++
				} else if rs[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j >= len(rs) {
				j = len(rs) - 1
			}
			buf.WriteString(string(rs[i : j+1]))
			inWord = true
			i = j
		case c == '\\' && i+1 < len(rs):
			if rs[i+1] != '\n' {
				buf.WriteRune(rs[i+1])
				inWord = true
			}
			i++
		case c == '<' && i+1 < len(rs) && rs[i+1] == '<' && !(i+2 < len(rs) && rs[i+2] == '<'):
			// Heredoc: remember its delimiter; the body starts at the next newline.
			endWord()
			j := i + 2
			if j < len(rs) && rs[j] == '-' {
				j++
			}
			for j < len(rs) && rs[j] == ' ' {
				j++
			}
			k := j
			for k < len(rs) && !strings.ContainsRune(" \t\n;|&)", rs[k]) {
				k++
			}
			if d := strings.Trim(string(rs[j:k]), `'"`); d != "" {
				heredoc = append(heredoc, d)
			}
			i = k - 1
		case c == '#' && !inWord:
			// A comment runs to the end of the line.
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case c == '\n':
			endCmd()
			for len(heredoc) > 0 && i < len(rs) {
				d := heredoc[0]
				heredoc = heredoc[1:]
				// Skip the body: i moves to the newline that ends the
				// delimiter line, or past the end if there is none.
				i = HeredocEnd(rs, i+1, d)
			}
		case c == '&' && ((i > 0 && rs[i-1] == '>') || (i+1 < len(rs) && rs[i+1] == '>')):
			// Part of a redirect: 2>&1, &>file.
			buf.WriteRune(c)
			inWord = true
		case c == ';' || c == '|' || c == '&':
			endCmd()
			if i+1 < len(rs) && (rs[i+1] == c) {
				i++
			}
		case c == ' ' || c == '\t' || c == '(' || c == ')' || c == '{' || c == '}':
			endWord()
		default:
			buf.WriteRune(c)
			inWord = true
		}
	}
	endCmd()
	return out
}

// HeredocEnd returns the index (in runes) of the newline ending the first
// line at or after from whose trimmed text is d, or len(rs) if none.
func HeredocEnd(rs []rune, from int, d string) int {
	start := from
	for j := from; j <= len(rs); j++ {
		if j == len(rs) || rs[j] == '\n' {
			if strings.TrimSpace(string(rs[start:j])) == d {
				return j
			}
			start = j + 1
		}
	}
	return len(rs)
}

var (
	AssignRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	RedirectRe = regexp.MustCompile(`^(\d*>>?|&>>?|\d*<|>&|\d+>&\d+|\d*>&-)`)
)

// Commands that only change the shell's own state. They are not work.
var ShellState = map[string]bool{"cd": true, "pushd": true, "popd": true, "set": true, "export": true, "unset": true, "source": true, ".": true, "shopt": true, "trap": true, "true": true, ":": true}

// Commands that only print. An agent uses them to label its own output.
var OutputOnly = map[string]bool{"echo": true, "printf": true}

// Shell grammar words. A leading one is removed from the command it
// introduces ("do git status" is git status); a command that is only grammar
// ("done", "fi", a "for x in ..." header) is dropped.
var (
	LeadingKeywords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true}
	GrammarOnly     = map[string]bool{"done": true, "fi": true, "esac": true, "for": true, "case": true, "select": true, "in": true, "function": true}
)

// Programs that run the program named after them.
var Wrappers = map[string]bool{"sudo": true, "time": true, "nohup": true, "env": true, "command": true, "exec": true, "xargs": true}

// SimpleCommands turns a command line into its working commands: environment
// assignments, redirections and wrappers removed, shell-state commands
// dropped. Each result starts with the program's base name.
func SimpleCommands(line string) [][]Word {
	var out [][]Word
	skipUntil := "" // inside a for/select header until "do", a case until "esac"
	for _, ws := range SplitShell(line) {
		if len(ws) == 0 {
			continue
		}
		first := ws[0].Text
		if skipUntil != "" {
			if first != skipUntil {
				continue
			}
			skipUntil = ""
			if first == "esac" {
				continue
			}
		}
		switch first {
		case "for", "select":
			// A loop header can span lines ("for r in\n a\n b; do"); its
			// words are data, not commands.
			if !ContainsWord(ws, "do") {
				skipUntil = "do"
			}
			continue
		case "case":
			skipUntil = "esac"
			continue
		}
		var kept []Word
		skipNext := false
		for _, w := range ws {
			if skipNext {
				skipNext = false
				continue
			}
			if !w.Quoted && RedirectRe.MatchString(w.Text) {
				// "> file" takes the next word; ">file" and "2>&1" do not.
				if m := RedirectRe.FindString(w.Text); m == w.Text && !strings.Contains(m, "&") {
					skipNext = true
				}
				continue
			}
			kept = append(kept, w)
		}
		for len(kept) > 0 && !kept[0].Quoted && LeadingKeywords[kept[0].Text] {
			kept = kept[1:]
		}
		if len(kept) > 0 && !kept[0].Quoted && GrammarOnly[kept[0].Text] {
			continue
		}
		for len(kept) > 0 && !kept[0].Quoted && AssignRe.MatchString(kept[0].Text) {
			kept = kept[1:]
		}
		for len(kept) > 0 && Wrappers[kept[0].Text] {
			kept = kept[1:]
			for len(kept) > 0 && (strings.HasPrefix(kept[0].Text, "-") || AssignRe.MatchString(kept[0].Text)) {
				kept = kept[1:]
			}
		}
		if len(kept) > 0 && kept[0].Text == "timeout" {
			kept = kept[1:]
			if len(kept) > 0 {
				kept = kept[1:]
			}
		}
		if len(kept) == 0 || ShellState[kept[0].Text] || OutputOnly[kept[0].Text] {
			continue
		}
		// A program is a name: not a variable ($repo), a glob (*.csv) or a
		// data file (notes.md), which are what a split loop or heredoc
		// leaves behind.
		if !ProgramName.MatchString(filepath.Base(kept[0].Text)) || DataFile.MatchString(kept[0].Text) {
			continue
		}
		kept[0].Text = filepath.Base(kept[0].Text)
		out = append(out, kept)
	}
	return out
}

// IsCompound reports a command line that is more than one plain command: two
// or more commands (a pipeline, a chain, a sequence, a cd before the work), a
// redirect, a heredoc, or a command substitution. Its meaning depends on the
// whole line, so it is replayed as recorded.
func IsCompound(line string) bool {
	if strings.Contains(line, "<<") || strings.Contains(line, "$(") || strings.Contains(line, "`") {
		return true
	}
	cmds := SplitShell(line)
	if len(cmds) > 1 {
		return true
	}
	for _, ws := range cmds {
		for _, w := range ws {
			if !w.Quoted && RedirectRe.MatchString(w.Text) {
				return true
			}
		}
	}
	return false
}

// Span is one word of a command line at its exact byte position, quotes
// included. body marks a heredoc body.
type Span struct {
	S, E   int
	Text   string
	Quoted bool
	Body   bool
}

// WordSpans cuts a command line into words at their positions, the same way
// SplitShell reads it: quotes and $(...) stay inside their word, and each
// heredoc body is one span.
func WordSpans(line string) []Span {
	var out []Span
	start, quoted, inWord := 0, false, false
	var text strings.Builder
	var heredoc []string
	end := func(i int) {
		if inWord {
			out = append(out, Span{S: start, E: i, Text: text.String(), Quoted: quoted})
		}
		text.Reset()
		quoted, inWord = false, false
	}
	begin := func(i int) {
		if !inWord {
			start, inWord = i, true
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			begin(i)
			j := i + 1
			for j < len(line) && line[j] != c {
				if line[j] == '\\' && c != '\'' {
					j++
				}
				j++
			}
			if j >= len(line) {
				j = len(line) - 1
			}
			text.WriteString(line[i+1 : max(i+1, j)])
			quoted = true
			i = j
		case c == '$' && i+1 < len(line) && line[i+1] == '(':
			begin(i)
			depth, j := 0, i+1
			for ; j < len(line); j++ {
				if line[j] == '(' {
					depth++
				} else if line[j] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			if j >= len(line) {
				j = len(line) - 1
			}
			text.WriteString(line[i : j+1])
			i = j
		case c == '\\' && i+1 < len(line):
			begin(i)
			text.WriteByte(line[i+1])
			i++
		case c == '<' && i+1 < len(line) && line[i+1] == '<':
			end(i)
			j := i + 2
			if j < len(line) && line[j] == '-' {
				j++
			}
			for j < len(line) && line[j] == ' ' {
				j++
			}
			k := j
			for k < len(line) && !strings.ContainsRune(" \t\n;|&)", rune(line[k])) {
				k++
			}
			if d := strings.Trim(line[j:k], `'"`); d != "" {
				heredoc = append(heredoc, d)
			}
			i = k - 1
		case c == '\n':
			end(i)
			for len(heredoc) > 0 && i+1 <= len(line) {
				d := heredoc[0]
				heredoc = heredoc[1:]
				bodyStart := i + 1
				bodyEnd, lineEnd := HeredocEndBytes(line, bodyStart, d)
				out = append(out, Span{S: bodyStart, E: bodyEnd, Text: line[bodyStart:bodyEnd], Body: true})
				// Continue after the line that ends the body.
				i = lineEnd
			}
		case c == '#' && !inWord:
			for i+1 < len(line) && line[i+1] != '\n' {
				i++
			}
		case strings.IndexByte(" \t;|&(){}", c) >= 0:
			end(i)
		default:
			begin(i)
			text.WriteByte(c)
		}
	}
	end(len(line))
	return out
}

// HeredocEndBytes finds the line holding exactly d at or after from: it
// returns where the body ends (that line's start) and where the line itself
// ends (its newline, or len(line)). With no such line the body runs to the
// end.
func HeredocEndBytes(line string, from int, d string) (bodyEnd, lineEnd int) {
	start := from
	for j := from; j <= len(line); j++ {
		if j == len(line) || line[j] == '\n' {
			if strings.TrimSpace(line[start:j]) == d {
				return start, j
			}
			start = j + 1
		}
	}
	return len(line), len(line)
}

// ShapeOf is the line with every word replaced by a marker: two lines with
// the same shape differ only in their words.
func ShapeOf(line string, ws []Span) string {
	var b strings.Builder
	last := 0
	for _, w := range ws {
		b.WriteString(line[last:w.S])
		if w.Body {
			b.WriteString("\x00B")
		} else {
			b.WriteString("\x00")
		}
		last = w.E
	}
	b.WriteString(line[last:])
	return b.String()
}

var (
	ProgramName = regexp.MustCompile(`^(\[|\[\[|[A-Za-z0-9_][A-Za-z0-9._+-]*)$`)
	DataFile    = regexp.MustCompile(`(?i)\.(md|csv|json|jsonl|txt|log|ya?ml|html?|xml|pdf|png|jpe?g|svg|tsv|toml|lock)$`)
)

func ContainsWord(ws []Word, w string) bool {
	for _, x := range ws {
		if !x.Quoted && x.Text == w {
			return true
		}
	}
	return false
}
