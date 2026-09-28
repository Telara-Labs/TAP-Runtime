package main

import "strings"

// reorderArgs moves every flag token (and, for flags that take a value, its
// following value token) before the remaining positional tokens, so
// `tap init <name> --web` works the same as `tap init --web <name>` --
// Go's flag.FlagSet stops parsing flags at the first positional argument,
// which would otherwise make flag placement order-sensitive in a way users
// (and agents driving this CLI non-interactively) shouldn't have to think
// about. valueFlags lists (without leading dashes) the flags that consume a
// following token as their value; anything else starting with "-" is
// treated as boolean.
func reorderArgs(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // "-flag=value" form already carries its value
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}
