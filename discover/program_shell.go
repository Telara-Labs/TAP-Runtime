package discover

import (
	"fmt"
	"strings"
	"unicode"
)

// programShellWords accepts only one literal argv invocation. The mining
// tokenizer is intentionally permissive; generation must not interpret shell
// control flow or expansion as inert arguments.
func programShellWords(line string) ([]string, error) {
	var words []string
	var buf strings.Builder
	quote := rune(0)
	inWord := false
	escaped := false
	for _, r := range line {
		if escaped {
			if r == '\n' {
				return nil, fmt.Errorf("line continuation is not a literal argv invocation")
			}
			buf.WriteRune(r)
			inWord, escaped = true, false
			continue
		}
		if quote == '\'' {
			if r == quote {
				quote = 0
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		if r == '\\' && quote == '"' {
			return nil, fmt.Errorf("quoted shell escapes are not a literal argv invocation")
		}
		if r == '\\' && quote == 0 {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else if r == '$' || r == '`' {
				return nil, fmt.Errorf("shell expansion is not a literal argv invocation")
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if r == '\n' || r == '\r' {
				return nil, fmt.Errorf("multiple shell lines are not one invocation")
			}
			if inWord {
				words = append(words, buf.String())
				buf.Reset()
				inWord = false
			}
		case strings.ContainsRune(";&|<>(){}$`*?[]~#", r):
			return nil, fmt.Errorf("shell control or expansion is not a literal argv invocation")
		default:
			buf.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated shell quote or escape")
	}
	if inWord {
		words = append(words, buf.String())
	}
	if len(words) == 0 || strings.Contains(words[0], "/") || strings.Contains(words[0], "=") {
		return nil, fmt.Errorf("command name must be a literal program name")
	}
	return words, nil
}

type programShellStage struct {
	Words     []string
	Connector string // empty for the first stage, then pipe or and
}

// programShellPlan accepts a direct command, a pipe, or a success-gated &&
// chain of literal commands. Mixed operators remain unresolved: shell pipe
// precedence and exit behavior cannot be inferred from a flat call trace.
func programShellPlan(line string) ([]programShellStage, error) {
	var segments []string
	var connectors []string
	start := 0
	quote := rune(0)
	escaped := false
	operator := ""
	for i, r := range line {
		if i < start {
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if quote == '\'' {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == '|' || r == '&' {
			if i > start && line[i-1] == byte(r) {
				continue
			}
			connector := "pipe"
			end := i + 1
			if r == '&' {
				if end >= len(line) || line[end] != '&' {
					return nil, fmt.Errorf("single shell ampersand is not a supported connector")
				}
				connector, end = "and", i+2
			}
			if i+1 < len(line) && line[i+1] == '|' || i > 0 && line[i-1] == '|' {
				return nil, fmt.Errorf("shell OR is not a data pipe")
			}
			if operator != "" && operator != connector {
				return nil, fmt.Errorf("mixed shell connectors are unresolved")
			}
			operator = connector
			segments = append(segments, strings.TrimSpace(line[start:i]))
			connectors = append(connectors, connector)
			start = end
		}
	}
	segments = append(segments, strings.TrimSpace(line[start:]))
	if len(segments) > 8 {
		return nil, fmt.Errorf("compound shell invocation has more than eight commands")
	}
	plan := make([]programShellStage, 0, len(segments))
	for i, segment := range segments {
		words, err := programShellWords(segment)
		if err != nil {
			return nil, err
		}
		stage := programShellStage{Words: words}
		if i > 0 {
			stage.Connector = connectors[i-1]
		}
		plan = append(plan, stage)
	}
	return plan, nil
}

func programShellCommands(line string) ([][]string, error) {
	plan, err := programShellPlan(line)
	if err != nil {
		return nil, err
	}
	commands := make([][]string, 0, len(plan))
	for _, stage := range plan {
		commands = append(commands, stage.Words)
	}
	return commands, nil
}

func programCommandRunsCode(name string, args []string) bool {
	switch name {
	case "bash", "sh", "zsh", "env", "xargs", "python", "python3", "node", "deno", "ruby", "perl", "php", "ssh", "sudo",
		"cd", "pushd", "popd", "source", ".", "export", "set", "unset", "alias", "unalias", "eval", "exec", "command", "builtin", "read", "trap", "umask", "ulimit":
		return true
	case "docker":
		return len(args) > 0 && (args[0] == "run" || args[0] == "exec")
	case "kubectl":
		return len(args) > 0 && (args[0] == "run" || args[0] == "exec")
	case "find":
		for _, arg := range args {
			if arg == "-exec" || arg == "-execdir" || arg == "-ok" || arg == "-okdir" {
				return true
			}
		}
	}
	return false
}
