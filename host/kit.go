package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"gopkg.in/yaml.v3"
)

// Docker Sandbox Kit export. `tap kit export` writes a mixin Kit, a
// descriptor and the dockerfile beside it, that puts this runner into a
// sandbox and lets the sandbox reach exactly the origins the chosen
// primitives declare under fetch. It writes files and nothing else: it does
// not build or push an image, and it declares no credentials, because TAP
// borrows the connections of the agent it runs in.

const (
	kitDescriptorName = "tap.yaml"
	kitDockerfileName = "tap.dockerfile"
	kitReleaseBase    = "https://github.com/Telara-Labs/TAP-Runtime/releases/download"
)

// kitVersionRE is the version shape the Kit specification accepts.
var kitVersionRE = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z-]+)*$`)

var kitMethodRE = regexp.MustCompile(`^[A-Z]+$`)

// kitPackage is one primitive whose fetch declarations go into the Kit.
type kitPackage struct {
	Name     string // publisher/name, for messages
	Manifest *mf.Manifest
}

// kitRule is one allow entry of the network policy: one host and the methods
// admitted to it.
type kitRule struct {
	Host    string
	Methods []string
}

// kitDescriptor mirrors the fields of the Kit descriptor that are written.
type kitDescriptor struct {
	SchemaVersion string          `yaml:"schemaVersion"`
	DisplayName   string          `yaml:"displayName"`
	Description   string          `yaml:"description"`
	SourceURL     string          `yaml:"sourceUrl"`
	Licenses      []string        `yaml:"licenses"`
	Kind          string          `yaml:"kind"`
	Version       string          `yaml:"version"`
	Provides      []string        `yaml:"provides"`
	Capabilities  []kitCapability `yaml:"capabilities,omitempty"`
	Dockerfile    string          `yaml:"dockerfile"`
}

type kitCapability struct {
	Type   string        `yaml:"type"`
	Config kitNetworkCfg `yaml:"config"`
}

type kitNetworkCfg struct {
	Runtime kitPhase `yaml:"runtime"`
}

type kitPhase struct {
	Allow []kitEntry `yaml:"allow"`
}

type kitEntry struct {
	Hosts   []string `yaml:"hosts,flow"`
	Methods []string `yaml:"methods,flow"`
}

// kitRules computes the exact union of the packages' fetch declarations: one
// rule per host, carrying every method any package declares for it. It
// refuses, naming package and origin, any origin the network policy cannot
// hold, rather than widening it. An origin with no methods means GET, as it
// does everywhere else in the runner.
func kitRules(pkgs []kitPackage) ([]kitRule, error) {
	byHost := map[string]map[string]bool{}
	var problems []string
	for _, p := range pkgs {
		for _, f := range p.Manifest.Fetch {
			host, why := kitHost(f.Origin)
			if why != "" {
				problems = append(problems, fmt.Sprintf("%s: fetch origin %q %s", p.Name, f.Origin, why))
				continue
			}
			methods := f.Methods
			if len(methods) == 0 {
				methods = []string{"GET"}
			}
			bad := false
			for _, m := range methods {
				if !kitMethodRE.MatchString(strings.ToUpper(m)) {
					problems = append(problems, fmt.Sprintf("%s: fetch origin %q declares method %q, which is not an HTTP method token", p.Name, f.Origin, m))
					bad = true
				}
			}
			if bad {
				continue
			}
			set := byHost[host]
			if set == nil {
				set = map[string]bool{}
				byHost[host] = set
			}
			for _, m := range methods {
				set[strings.ToUpper(m)] = true
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("the sandbox network policy cannot express these fetch declarations, and the Kit does not widen them:\n  %s", strings.Join(problems, "\n  "))
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	rules := make([]kitRule, 0, len(hosts))
	for _, h := range hosts {
		var ms []string
		if byHost[h]["ANY"] {
			ms = []string{"ANY"}
		} else {
			for m := range byHost[h] {
				ms = append(ms, m)
			}
			sort.Strings(ms)
		}
		rules = append(rules, kitRule{Host: h, Methods: ms})
	}
	return rules, nil
}

// kitHost turns a fetch origin into the host the network policy names, or
// says why it cannot. The policy holds a literal host: it cannot say "https
// only" or "this port only", and a bounded entry cannot hold a wildcard.
func kitHost(origin string) (string, string) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", "is not a scheme and a host"
	}
	if u.Scheme != "https" {
		return "", "uses " + u.Scheme + ", and the network policy cannot restrict a host to one scheme; only https origins are exported"
	}
	if strings.Contains(u.Hostname(), "*") {
		return "", "uses a wildcard host, and the network policy only bounds literal hosts to methods"
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", "names port " + port + ", and the network policy ignores ports, so it would allow the host on every port"
	}
	return strings.ToLower(u.Hostname()), ""
}

// kitFiles renders the descriptor and the dockerfile for a runner version.
func kitFiles(version string, pkgs []kitPackage) (descriptor, dockerfile []byte, err error) {
	if !kitVersionRE.MatchString(version) {
		return nil, nil, fmt.Errorf("this runner is %q, which is not a release version; export from an installed release of tap", version)
	}
	rules, err := kitRules(pkgs)
	if err != nil {
		return nil, nil, err
	}
	d := kitDescriptor{
		SchemaVersion: "3",
		DisplayName:   "TAP runner",
		Description:   "The TAP runner, with network access limited to the origins its primitives declare",
		SourceURL:     "https://github.com/Telara-Labs/TAP-Runtime",
		Licenses:      []string{"MIT"},
		Kind:          "mixin",
		Version:       version,
		Provides:      []string{"tap@" + version},
		Dockerfile:    kitDockerfileName,
	}
	if len(rules) > 0 {
		entries := make([]kitEntry, len(rules))
		for i, r := range rules {
			entries[i] = kitEntry{Hosts: []string{r.Host}, Methods: r.Methods}
		}
		d.Capabilities = []kitCapability{{
			Type:   "com.docker.sandbox/network-policy@2",
			Config: kitNetworkCfg{Runtime: kitPhase{Allow: entries}},
		}}
	}
	var buf bytes.Buffer
	buf.WriteString("# syntax=docker/sandbox-kit:3\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, nil, err
	}
	df := strings.ReplaceAll(kitDockerfileTemplate, "@VERSION@", version)
	df = strings.ReplaceAll(df, "@BASE@", kitReleaseBase)
	return buf.Bytes(), []byte(df), nil
}

const kitDockerfileTemplate = `# syntax=docker/dockerfile:1
# The TAP runner @VERSION@ as an overlay. The released binary is checked against
# the release's SHA256SUMS before it is installed. The interpreters the runner
# would otherwise download on first use are fetched here, at build time, so the
# sandbox needs no network access beyond the primitives' own fetch origins.
FROM alpine:3.22 AS fetch
ARG TARGETARCH
RUN apk add --no-cache curl
WORKDIR /work
RUN set -eu; \
    base=@BASE@/v@VERSION@; \
    name=tap-@VERSION@-linux-${TARGETARCH}; \
    curl -fsSLO "$base/SHA256SUMS"; \
    curl -fsSLO "$base/$name"; \
    grep " $name\$" SHA256SUMS | sha256sum -c -; \
    install -D -m 0755 "$name" /out/usr/local/bin/tap; \
    /out/usr/local/bin/tap fetch --interpreters /out/home/agent/.cache/tap-runtime/interpreters; \
    chown -R 1000:1000 /out/home/agent

FROM scratch
COPY --from=fetch /out /
`

const kitUsage = "usage: tap kit export [--out DIR] [<package-dir>...]\n"

// kitCommand is `tap kit`.
func kitCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "export" {
		fmt.Fprint(stderr, kitUsage)
		return 2
	}
	fs := flag.NewFlagSet("kit export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "tap-kit", "directory to write the Kit into")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	pkgs, err := kitPackages(fs.Args(), nil)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	desc, df, err := kitFiles(version, pkgs)
	if err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(stderr, "tap:", err)
		return 1
	}
	for name, b := range map[string][]byte{kitDescriptorName: desc, kitDockerfileName: df} {
		if err := os.WriteFile(filepath.Join(*out, name), b, 0o644); err != nil {
			fmt.Fprintln(stderr, "tap:", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "wrote %s and %s in %s for %d primitive(s)\n", kitDescriptorName, kitDockerfileName, *out, len(pkgs))
	return 0
}

// kitPackages loads the named package directories, or, with none named,
// every primitive installed in the local catalog. Saved version history is
// not installed: only the current copy of each primitive counts.
func kitPackages(dirs []string, catalog func() ([]catalogEntry, error)) ([]kitPackage, error) {
	var pkgs []kitPackage
	if len(dirs) == 0 {
		entries, err := localCatalog()
		if err != nil {
			return nil, err
		}
		sep := string(filepath.Separator)
		for _, e := range entries {
			if strings.Contains(e.Path, sep+".versions"+sep) {
				continue
			}
			pkgs = append(pkgs, kitPackage{Name: e.Ref, Manifest: e.Manifest})
		}
		sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
		return pkgs, nil
	}
	for _, dir := range dirs {
		digest, m, err := packageDigest(dir)
		if err != nil {
			return nil, err
		}
		if problems := m.RunProblems(); len(problems) > 0 {
			return nil, fmt.Errorf("%s cannot be run: %s", dir, strings.Join(problems, "; "))
		}
		name := m.Metadata.Publisher + "/" + m.Metadata.Name
		pkgs = append(pkgs, kitPackage{Name: name, Manifest: m})
		if len(m.Uses) == 0 {
			continue
		}
		// What a package uses runs inside the same sandbox, so its fetch
		// origins are needed too, at any depth. A pinned child that is not
		// installed is refused as a run would refuse it.
		entries, err := localEntries(catalog)
		if err != nil {
			return nil, err
		}
		nodes, err := resolveUses(entries, m, digest)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, n := range flattenUses(nodes, "") {
			pkgs = append(pkgs, kitPackage{Name: name + " uses " + n.Name + " (" + n.Ref + ")", Manifest: n.Manifest})
		}
	}
	return pkgs, nil
}
