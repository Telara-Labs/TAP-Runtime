package discover

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"

	"gitlab.com/telara-labs/tap-runtime/discover/pyparse"
)

// Temporary: compares the Go recognizer with the Python one on real and
// synthetic inputs. Not committed.
func TestZZParityGoVsPython(t *testing.T) {
	home, _ := os.UserHomeDir()
	var bodies []string
	readers := []trace.Reader{history.ClaudeCode{Dir: filepath.Join(home, ".claude", "projects")}}
	for _, r := range readers {
		ss, err := r.Read(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			for _, c := range s.Calls {
				if c.Tool != "shell" {
					continue
				}
				if b, _, ok := pyparse.InlinePythonBody(c.Command); ok {
					bodies = append(bodies, b)
				}
			}
		}
	}
	syn := []string{
		"p = 'a.txt'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = 'a.txt'\nt = open(p).read()\nt = t.replace('x', 'y')\nopen(p, 'w').write(t)",
		"p='a.txt'; t=open(p).read(); u=t.replace('x','y'); open(p,'w').write(u)",
		"p = 'a.txt'  # c\n\n# x\nt = open( p ).read()\nu = t.replace(\n  'x',\n  'y'\n)\nopen(p, \"w\").write(u)\n",
		"p = 'a.txt'\nt = open(p).read()\nu = t.replace('', 'y')\nopen(p, 'w').write(u)",
		"p = 'a.txt'\nt = open(p).read()\nu = t.replace('\\\n', 'y')\nopen(p, 'w').write(u)",
		"p = r'a\\b'\nt = open(p).read()\nu = t.replace(r'\\x', u'y')\nopen(p, 'w').write(u)",
		"p = b'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = f'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = 'a' 'b'\nt = open(p).read()\nu = t.replace('x' 'z', 'y')\nopen(p, 'w').write(u)",
		"p = ('a')\nt = (open(p)).read()\nu = (t).replace(('x'), 'y')\n(open(p, 'w')).write((u))",
		"p = 'a'\nt = open(p,).read()\nu = t.replace('x', 'y',)\nopen(p, 'w',).write(u,)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, '\\x77').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'a').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, mode='w').write(u)",
		"  p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)\nprint(1)",
		"p = '''a\nb'''\nt = open(p).read()\nu = t.replace(\"\"\"x\"\"\", 'y')\nopen(p, 'w').write(u)",
		"open = 'a'\nt = open(open).read()\nu = t.replace('x', 'y')\nopen(open, 'w').write(u)",
		"p = 'a'\np2 = open(p).read()\np = p2.replace('x', 'y')\nopen(p, 'w').write(p)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u);",
		"p = 'a';;\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y') \\\n\nopen(p, 'w').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('\\N{BULLET}', 'y')\nopen(p, 'w').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('é', 'ü')\nopen(p, 'w').write(u)",
		"pé = 'a'\nt = open(pé).read()\nu = t.replace('x', 'y')\nopen(pé, 'w').write(u)",
		"p = 'a'\nif = open(p).read()\nu = if.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)\n\n\n",
		"p = 'a'\r\nt = open(p).read()\r\nu = t.replace('x', 'y')\r\nopen(p, 'w').write(u)\r\n",
		"p = 'a'\nt = open(p).read()\nu = t.replace('x', 'y')\nopen(p, 'w').write(u)\n  # trailing indented comment",
		"p = 'a'\nt = open(p).read()\n\tu = t.replace('x', 'y')\nopen(p, 'w').write(u)",
		"p = 'a'\nt = open(p).read()\nu = t.replace('\\'', '\"')\nopen(p, 'w').write(u)",
	}
	all := append(bodies, syn...)
	accepted, diff := 0, 0
	for i, b := range all {
		g, py := pyparse.StrictInlineFileReplace(b), codegen.StrictInlineFileReplacePy(b)
		if py {
			accepted++
		}
		if g != py {
			diff++
			src := "recorded"
			if i >= len(bodies) {
				src = "synthetic"
			}
			t.Errorf("%s #%d go=%v python=%v:\n%s", src, i, g, py, b)
		}
	}
	t.Logf("compared %d recorded + %d synthetic bodies; python accepted %d; disagreements %d", len(bodies), len(syn), accepted, diff)
}
