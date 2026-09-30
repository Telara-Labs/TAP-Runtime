package discover

import (
	"path/filepath"
	"regexp"
	"strings"
)

// word is one shell word. Quoted records that any part of it was quoted, which
// is how free text (a commit message, a search pattern) is told from a word.
type word struct {
	Text   string
	Quoted bool
}

// splitShell breaks a command line into simple commands at &&, ||, ;, | and
// newlines outside quotes, and each simple command into words. Heredoc bodies
// are dropped; the command that reads them is kept. $(...) and backquotes stay
// inside the word they appear in. This is a tokenizer for counting, not a
// shell: a malformed line still yields its best-effort words.
func splitShell(line string) [][]word {
	var (
		out     [][]word
		cur     []word
		buf     strings.Builder
		quoted  bool
		inWord  bool
		heredoc []string
	)
	endWord := func() {
		if inWord {
			cur = append(cur, word{Text: buf.String(), Quoted: quoted})
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
		case c == '\n':
			endCmd()
			for len(heredoc) > 0 && i < len(rs) {
				d := heredoc[0]
				heredoc = heredoc[1:]
				// Skip the body: i moves to the newline that ends the
				// delimiter line, or past the end if there is none.
				i = heredocEnd(rs, i+1, d)
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

// heredocEnd returns the index (in runes) of the newline ending the first
// line at or after from whose trimmed text is d, or len(rs) if none.
func heredocEnd(rs []rune, from int, d string) int {
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
	assignRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	redirectRe = regexp.MustCompile(`^(\d*>>?|&>>?|\d*<|>&|\d+>&\d+|\d*>&-)`)
)

// Commands that only change the shell's own state. They are not work.
var shellState = map[string]bool{"cd": true, "pushd": true, "popd": true, "set": true, "export": true, "unset": true, "source": true, ".": true, "shopt": true, "trap": true, "true": true, ":": true}

// Commands that only print. An agent uses them to label its own output.
var outputOnly = map[string]bool{"echo": true, "printf": true}

// Shell grammar words. A leading one is removed from the command it
// introduces ("do git status" is git status); a command that is only grammar
// ("done", "fi", a "for x in ..." header) is dropped.
var (
	leadingKeywords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true}
	grammarOnly     = map[string]bool{"done": true, "fi": true, "esac": true, "for": true, "case": true, "select": true, "in": true, "function": true}
)

// Programs that run the program named after them.
var wrappers = map[string]bool{"sudo": true, "time": true, "nohup": true, "env": true, "command": true, "exec": true, "xargs": true}

// simpleCommands turns a command line into its working commands: environment
// assignments, redirections and wrappers removed, shell-state commands
// dropped. Each result starts with the program's base name.
func simpleCommands(line string) [][]word {
	var out [][]word
	for _, ws := range splitShell(line) {
		var kept []word
		skipNext := false
		for _, w := range ws {
			if skipNext {
				skipNext = false
				continue
			}
			if !w.Quoted && redirectRe.MatchString(w.Text) {
				// "> file" takes the next word; ">file" and "2>&1" do not.
				if m := redirectRe.FindString(w.Text); m == w.Text && !strings.Contains(m, "&") {
					skipNext = true
				}
				continue
			}
			kept = append(kept, w)
		}
		for len(kept) > 0 && !kept[0].Quoted && leadingKeywords[kept[0].Text] {
			kept = kept[1:]
		}
		if len(kept) > 0 && !kept[0].Quoted && grammarOnly[kept[0].Text] {
			continue
		}
		for len(kept) > 0 && !kept[0].Quoted && assignRe.MatchString(kept[0].Text) {
			kept = kept[1:]
		}
		for len(kept) > 0 && wrappers[kept[0].Text] {
			kept = kept[1:]
			for len(kept) > 0 && (strings.HasPrefix(kept[0].Text, "-") || assignRe.MatchString(kept[0].Text)) {
				kept = kept[1:]
			}
		}
		if len(kept) > 0 && kept[0].Text == "timeout" {
			kept = kept[1:]
			if len(kept) > 0 {
				kept = kept[1:]
			}
		}
		if len(kept) == 0 || shellState[kept[0].Text] || outputOnly[kept[0].Text] {
			continue
		}
		kept[0].Text = filepath.Base(kept[0].Text)
		out = append(out, kept)
	}
	return out
}

// isCompound reports a command line that is more than one plain command: two
// or more commands (a pipeline, a chain, a sequence, a cd before the work), a
// redirect, a heredoc, or a command substitution. Its meaning depends on the
// whole line, so it is replayed as recorded.
func isCompound(line string) bool {
	if strings.Contains(line, "<<") || strings.Contains(line, "$(") || strings.Contains(line, "`") {
		return true
	}
	cmds := splitShell(line)
	if len(cmds) > 1 {
		return true
	}
	for _, ws := range cmds {
		for _, w := range ws {
			if !w.Quoted && redirectRe.MatchString(w.Text) {
				return true
			}
		}
	}
	return false
}

// span is one word of a command line at its exact byte position, quotes
// included. body marks a heredoc body.
type span struct {
	s, e   int
	text   string
	quoted bool
	body   bool
}

// wordSpans cuts a command line into words at their positions, the same way
// splitShell reads it: quotes and $(...) stay inside their word, and each
// heredoc body is one span.
func wordSpans(line string) []span {
	var out []span
	start, quoted, inWord := 0, false, false
	var text strings.Builder
	var heredoc []string
	end := func(i int) {
		if inWord {
			out = append(out, span{s: start, e: i, text: text.String(), quoted: quoted})
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
				bodyEnd, lineEnd := heredocEndBytes(line, bodyStart, d)
				out = append(out, span{s: bodyStart, e: bodyEnd, text: line[bodyStart:bodyEnd], body: true})
				// Continue after the line that ends the body.
				i = lineEnd
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

// heredocEndBytes finds the line holding exactly d at or after from: it
// returns where the body ends (that line's start) and where the line itself
// ends (its newline, or len(line)). With no such line the body runs to the
// end.
func heredocEndBytes(line string, from int, d string) (bodyEnd, lineEnd int) {
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

// shapeOf is the line with every word replaced by a marker: two lines with
// the same shape differ only in their words.
func shapeOf(line string, ws []span) string {
	var b strings.Builder
	last := 0
	for _, w := range ws {
		b.WriteString(line[last:w.s])
		if w.body {
			b.WriteString("\x00B")
		} else {
			b.WriteString("\x00")
		}
		last = w.e
	}
	b.WriteString(line[last:])
	return b.String()
}
