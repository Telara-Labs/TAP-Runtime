package main

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// stripTypes turns TypeScript into the JavaScript the interpreter runs. It
// removes the types and checks the syntax. It does NOT check the types: a
// program whose types are wrong and whose syntax is right is run.
//
// This happens in the runner, before the sandbox, on source the package
// digest already covers. The program that reaches the sandbox is derived
// from the file the author shipped and from nothing else.
func stripTypes(source, name string) (string, error) {
	r := api.Transform(source, api.TransformOptions{
		Loader:     api.LoaderTS,
		Target:     api.ES2020,
		Format:     api.FormatDefault,
		Sourcefile: name,
		// The interpreter evaluates one script. Nothing is bundled and
		// nothing is imported, so a package cannot pull in a file the
		// digest does not cover.
	})
	if len(r.Errors) > 0 {
		var lines []string
		for _, e := range r.Errors {
			at := ""
			if e.Location != nil {
				at = fmt.Sprintf("%s:%d:%d: ", e.Location.File, e.Location.Line, e.Location.Column)
			}
			lines = append(lines, at+e.Text)
		}
		return "", fmt.Errorf("%s is not valid TypeScript:\n  %s", name, strings.Join(lines, "\n  "))
	}
	return string(r.Code), nil
}
