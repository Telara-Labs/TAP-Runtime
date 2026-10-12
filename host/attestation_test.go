package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const attestedManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: com.acme.tools, name: greeter, version: 1.2.0, description: Greet a person}
execution: {entrypoint: main.sh}
`

// registry plays the key holder: a key pair made for the test, never stored.
type registry struct {
	priv ed25519.PrivateKey
	root publisherRoot
}

func newRegistry(t *testing.T, prefix, keyID string) registry {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return registry{priv: priv, root: publisherRoot{
		PublisherPrefix: prefix, KeyID: keyID, Alg: "ed25519", PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
	}}
}

// payloadJSON is what the registry signs. runDigest is the package's run
// digest; empty leaves the field out, as an attestation signed before content
// binding does.
func payloadJSON(publisher, name, version, artifact, runDigest string, validated bool) []byte {
	m := map[string]any{
		"validated": validated, "artifact_digest": artifact, "publisher": publisher, "name": name, "version": version,
	}
	if runDigest != "" {
		m["run_digest"] = runDigest
	}
	b, _ := json.Marshal(m)
	return b
}

func runDigestOf(t *testing.T, dir string) string {
	t.Helper()
	d, _, err := packageDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// attest writes ATTESTATION.json and ATTESTATION.sig into dir the way the
// installer does, signed by r.
func (r registry) attest(t *testing.T, dir string, payload []byte) {
	t.Helper()
	sig, _ := json.Marshal(map[string]string{
		"key_id": r.root.KeyID, "alg": "ed25519", "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(r.priv, payload)),
	})
	if err := os.WriteFile(filepath.Join(dir, attestationFile), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attestationSigFile), sig, 0o644); err != nil {
		t.Fatal(err)
	}
}

// goodPayload is a genuine attestation of the package in dir.
func goodPayload(t *testing.T, dir string) []byte {
	return payloadJSON("com.acme.tools", "greeter", "1.2.0", "sha256:aa", runDigestOf(t, dir), true)
}

func attestedPackage(t *testing.T) string {
	t.Helper()
	dir := writePackage(t, attestedManifest, "echo hello\n")
	if err := os.WriteFile(filepath.Join(dir, ".telara-primitive.json"),
		[]byte(`{"ref":"com.acme.tools/greeter@1.2.0","artifact_digest":"sha256:aa"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func check(t *testing.T, dir string, roots ...publisherRoot) attestationResult {
	t.Helper()
	_, m, err := packageDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return checkAttestation(dir, m, roots)
}

func TestAttestationVerifiedByATrustedKey(t *testing.T) {
	dir, reg := attestedPackage(t), newRegistry(t, "com.acme", "tap-attestation:v1")
	reg.attest(t, dir, goodPayload(t, dir))
	r := check(t, dir, reg.root)
	if r.State != attestationVerified || r.KeyID != "tap-attestation:v1" || r.label() != "verified by tap-attestation:v1" {
		t.Fatalf("result %+v label %q", r, r.label())
	}
}

func TestAttestationAbsentChangesNothing(t *testing.T) {
	dir := attestedPackage(t)
	if r := check(t, dir); r.State != attestationAbsent || r.label() != "" {
		t.Fatalf("result %+v", r)
	}
}

func TestAttestationWithoutATrustedKeyIsUnverifiedNotRefused(t *testing.T) {
	dir, reg := attestedPackage(t), newRegistry(t, "com.acme", "tap-attestation:v1")
	reg.attest(t, dir, goodPayload(t, dir))
	other := newRegistry(t, "com.acme", "another-key:v1")
	elsewhere := newRegistry(t, "com.other", "tap-attestation:v1")
	elsewhere.root.PublicKeyB64 = reg.root.PublicKeyB64
	for name, roots := range map[string][]publisherRoot{
		"no roots":                          nil,
		"another key id":                    {other.root},
		"key trusted for another publisher": {elsewhere.root},
	} {
		t.Run(name, func(t *testing.T) {
			r := check(t, dir, roots...)
			if r.State != attestationUnverified || !strings.Contains(r.label(), "not verified") {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

func TestAttestationInvalidIsRefused(t *testing.T) {
	good := newRegistry(t, "com.acme", "tap-attestation:v1")
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		roots func() []publisherRoot
		want  string
	}{
		{"signed by a different key under the same id", func(t *testing.T, dir string) {
			newRegistry(t, "com.acme", "tap-attestation:v1").attest(t, dir, goodPayload(t, dir))
		}, func() []publisherRoot { return []publisherRoot{good.root} }, "does not verify"},
		{"payload changed after signing", func(t *testing.T, dir string) {
			good.attest(t, dir, goodPayload(t, dir))
			os.WriteFile(filepath.Join(dir, attestationFile), append(goodPayload(t, dir), ' '), 0o644)
		}, func() []publisherRoot { return []publisherRoot{good.root} }, "does not verify"},
		{"attests another name", func(t *testing.T, dir string) {
			good.attest(t, dir, payloadJSON("com.acme.tools", "other", "1.2.0", "sha256:aa", runDigestOf(t, dir), true))
		}, func() []publisherRoot { return []publisherRoot{good.root} }, "name"},
		{"attests another version", func(t *testing.T, dir string) {
			good.attest(t, dir, payloadJSON("com.acme.tools", "greeter", "9.9.9", "sha256:aa", runDigestOf(t, dir), true))
		}, nil, "version"},
		{"attests another publisher", func(t *testing.T, dir string) {
			good.attest(t, dir, payloadJSON("com.evil", "greeter", "1.2.0", "sha256:aa", runDigestOf(t, dir), true))
		}, nil, "publisher"},
		{"attests another artifact digest", func(t *testing.T, dir string) {
			good.attest(t, dir, payloadJSON("com.acme.tools", "greeter", "1.2.0", "sha256:bb", runDigestOf(t, dir), true))
		}, nil, "artifact digest"},
		{"publish did not validate", func(t *testing.T, dir string) {
			good.attest(t, dir, payloadJSON("com.acme.tools", "greeter", "1.2.0", "sha256:aa", runDigestOf(t, dir), false))
		}, nil, "validation"},
		{"signature file missing", func(t *testing.T, dir string) {
			good.attest(t, dir, goodPayload(t, dir))
			os.Remove(filepath.Join(dir, attestationSigFile))
		}, nil, attestationSigFile},
		{"payload file missing", func(t *testing.T, dir string) {
			good.attest(t, dir, goodPayload(t, dir))
			os.Remove(filepath.Join(dir, attestationFile))
		}, nil, attestationFile},
		{"signature is not base64", func(t *testing.T, dir string) {
			good.attest(t, dir, goodPayload(t, dir))
			os.WriteFile(filepath.Join(dir, attestationSigFile), []byte(`{"key_id":"k","alg":"ed25519","signature":"!!"}`), 0o644)
		}, nil, "base64"},
		{"unsupported algorithm", func(t *testing.T, dir string) {
			good.attest(t, dir, goodPayload(t, dir))
			os.WriteFile(filepath.Join(dir, attestationSigFile), []byte(`{"key_id":"k","alg":"rsa","signature":"AA=="}`), 0o644)
		}, nil, "not supported"},
		{"attestation is a symlink", func(t *testing.T, dir string) {
			good.attest(t, dir, goodPayload(t, dir))
			target := filepath.Join(t.TempDir(), "elsewhere.json")
			os.WriteFile(target, goodPayload(t, dir), 0o644)
			os.Remove(filepath.Join(dir, attestationFile))
			os.Symlink(target, filepath.Join(dir, attestationFile))
		}, nil, "regular file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := attestedPackage(t)
			c.setup(t, dir)
			var roots []publisherRoot
			if c.roots != nil {
				roots = c.roots()
			}
			r := check(t, dir, roots...)
			if r.State != attestationInvalid || !strings.Contains(r.Reason, c.want) {
				t.Fatalf("result %+v, want invalid mentioning %q", r, c.want)
			}
		})
	}
}

func TestPublisherPrefixCoversOnADotBoundary(t *testing.T) {
	for prefix, want := range map[string]bool{"com.acme": true, "com.acme.tools": true, "com.acme.tool": false, "com.acmex": false, "com": true, "org": false} {
		if got := prefixCovers(prefix, "com.acme.tools"); got != want {
			t.Errorf("prefixCovers(%q) = %v, want %v", prefix, got, want)
		}
	}
}

// The runner is given a pulled package the way a server hands it: files on
// disk beside the saved-primitive marker, listed by tap_search.
func stageAttested(t *testing.T, c *client, reg registry, mutate func(dir string)) (dir string, identity map[string]any) {
	t.Helper()
	pkg := attestedPackage(t)
	identity = c.stagePackage(pkg)
	sum := sha256.Sum256([]byte(pkg))
	dir = filepath.Join(c.catalogRoot, hex.EncodeToString(sum[:8]))
	if err := os.WriteFile(filepath.Join(dir, ".telara-primitive.json"),
		[]byte(`{"ref":"com.acme.tools/greeter@1.2.0","artifact_digest":"sha256:aa"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.attest(t, dir, goodPayload(t, dir))
	if mutate != nil {
		mutate(dir)
	}
	return dir, identity
}

func trustKey(t *testing.T, r registry) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := trustPublisherCommand([]string{"add", "--publisher", r.root.PublisherPrefix, "--key-id", r.root.KeyID, "--public-key", r.root.PublicKeyB64}, &out, &errb); code != 0 {
		t.Fatalf("trust-publisher add: %d %s", code, &errb)
	}
}

func searchLabel(t *testing.T, c *client) string {
	t.Helper()
	search := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
	matches, _ := search["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("search = %#v", search)
	}
	label, _ := matches[0].(map[string]any)["attestation"].(string)
	return label
}

func TestAdmissionShowsVerifiedInSearchAndLoadAndRuns(t *testing.T) {
	c := startServer(t, true, accept)
	reg := newRegistry(t, "com.acme", "tap-attestation:v1")
	trustKey(t, reg)
	_, identity := stageAttested(t, c, reg, nil)

	if got := searchLabel(t, c); got != "verified by tap-attestation:v1" {
		t.Fatalf("tap_search attestation = %q", got)
	}
	loaded := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_load", "arguments": identity}))
	if loaded["attestation"] != "verified by tap-attestation:v1" {
		t.Fatalf("tap_load = %#v", loaded)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if run["isError"] == true || !strings.Contains(toolText(t, run), "hello") {
		t.Fatalf("a verified package must run: %#v", run)
	}
}

func TestAdmissionRefusesAnInvalidAttestation(t *testing.T) {
	c := startServer(t, true, accept)
	reg := newRegistry(t, "com.acme", "tap-attestation:v1")
	trustKey(t, reg)
	_, identity := stageAttested(t, c, reg, func(dir string) {
		os.WriteFile(filepath.Join(dir, attestationFile), append(goodPayload(t, dir), ' '), 0o644) // tampered after signing
	})

	if got := searchLabel(t, c); !strings.HasPrefix(got, "attestation invalid") {
		t.Fatalf("tap_search attestation = %q", got)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if run["isError"] != true || !strings.Contains(toolText(t, run), "refused") || !strings.Contains(toolText(t, run), "does not verify") {
		t.Fatalf("an invalid attestation must refuse the run: %#v", run)
	}
}

func TestAdmissionLeavesAPackageWithNoAttestationAlone(t *testing.T) {
	c := startServer(t, true, accept)
	pkg := attestedPackage(t)
	identity := c.stagePackage(pkg)
	if got := searchLabel(t, c); got != "" {
		t.Fatalf("a package with no attestation shows %q", got)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if run["isError"] == true || !strings.Contains(toolText(t, run), "hello") {
		t.Fatalf("run = %#v", run)
	}
}

func TestAdmissionTreatsAnUntrustedKeyAsUnsigned(t *testing.T) {
	c := startServer(t, true, accept)
	reg := newRegistry(t, "com.acme", "tap-attestation:v1")
	_, identity := stageAttested(t, c, reg, nil) // no trust-publisher add
	if got := searchLabel(t, c); !strings.Contains(got, "not verified") {
		t.Fatalf("tap_search attestation = %q", got)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if run["isError"] == true {
		t.Fatalf("an attestation with no trusted key must not stop a run: %#v", run)
	}
}

func TestTrustPublisherRoundTrip(t *testing.T) {
	cfg, old := t.TempDir(), userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = old })
	reg := newRegistry(t, "com.acme", "tap-attestation:v1")

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := trustPublisherCommand(args, &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, out, _ := run("list"); code != 0 || !strings.Contains(out, "no publishers are trusted") {
		t.Fatalf("empty list: %d %q", code, out)
	}
	add := []string{"add", "--publisher", "com.acme", "--key-id", "tap-attestation:v1", "--public-key", reg.root.PublicKeyB64}
	if code, _, e := run(add...); code != 0 {
		t.Fatalf("add: %d %s", code, e)
	}
	if code, _, _ := run(add...); code != 0 {
		t.Fatal("adding the same key twice must be harmless")
	}
	_, out, _ := run("list")
	if strings.Count(out, "tap-attestation:v1") != 1 || !strings.Contains(out, "com.acme") || !strings.Contains(out, reg.root.PublicKeyB64) {
		t.Fatalf("list = %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(cfg, "tap", "trusted_publishers.json"))
	if err != nil || !strings.Contains(string(raw), `"trusted_publishers"`) {
		t.Fatalf("config file %s: %v", raw, err)
	}
	if code, out, _ := run("remove", "--key-id", "tap-attestation:v1"); code != 0 || !strings.Contains(out, "removed 1") {
		t.Fatalf("remove: %d %q", code, out)
	}
	if _, out, _ := run("list"); !strings.Contains(out, "no publishers are trusted") {
		t.Fatalf("after remove: %q", out)
	}
}

func TestTrustPublisherRefusesBadInput(t *testing.T) {
	cfg, old := t.TempDir(), userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = old })
	reg := newRegistry(t, "com.acme", "k")
	for name, args := range map[string][]string{
		"no subcommand":     {},
		"unknown":           {"frobnicate"},
		"wildcard prefix":   {"add", "--publisher", "com.*", "--key-id", "k", "--public-key", reg.root.PublicKeyB64},
		"empty prefix":      {"add", "--key-id", "k", "--public-key", reg.root.PublicKeyB64},
		"short key":         {"add", "--publisher", "com.acme", "--key-id", "k", "--public-key", "AAAA"},
		"unsupported alg":   {"add", "--publisher", "com.acme", "--key-id", "k", "--alg", "rsa", "--public-key", reg.root.PublicKeyB64},
		"remove without id": {"remove"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := trustPublisherCommand(args, &out, &errb); code == 0 {
				t.Fatalf("accepted: %s", &out)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(cfg, "tap", "trusted_publishers.json")); !os.IsNotExist(err) {
		t.Fatal("a refused add must write nothing")
	}
}

func TestAttestationBindsTheRunDigestOfThePackage(t *testing.T) {
	reg := newRegistry(t, "com.acme", "tap-attestation:v1")

	t.Run("matching digest verifies", func(t *testing.T) {
		dir := attestedPackage(t)
		reg.attest(t, dir, goodPayload(t, dir))
		if r := check(t, dir, reg.root); r.State != attestationVerified {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("files swapped under a real attestation are refused", func(t *testing.T) {
		dir := attestedPackage(t)
		reg.attest(t, dir, goodPayload(t, dir)) // a genuine, correctly signed attestation
		if err := os.WriteFile(filepath.Join(dir, "main.sh"), []byte("echo swapped\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, roots := range map[string][]publisherRoot{"key trusted": {reg.root}, "no key trusted": nil} {
			r := check(t, dir, roots...)
			if r.State != attestationInvalid || !strings.Contains(r.Reason, "run digest") {
				t.Fatalf("%s: result %+v, want invalid on the run digest", name, r)
			}
		}
	})

	t.Run("an edited manifest is refused too", func(t *testing.T) {
		dir := attestedPackage(t)
		reg.attest(t, dir, goodPayload(t, dir))
		edited := strings.Replace(attestedManifest, "Greet a person", "Greet a person and read mail", 1)
		if err := os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := check(t, dir, reg.root); r.State != attestationInvalid || !strings.Contains(r.Reason, "run digest") {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("no run digest means signed before content binding", func(t *testing.T) {
		dir := attestedPackage(t)
		reg.attest(t, dir, payloadJSON("com.acme.tools", "greeter", "1.2.0", "sha256:aa", "", true))
		r := check(t, dir, reg.root)
		if r.State != attestationUnverified || !strings.Contains(r.Reason, "signed before content binding") {
			t.Fatalf("result %+v", r)
		}
		if !strings.HasPrefix(r.label(), "attestation present, not verified: signed before content binding") {
			t.Fatalf("label %q", r.label())
		}
	})
}

func TestAdmissionRefusesFilesSwappedUnderARealAttestation(t *testing.T) {
	c := startServer(t, true, accept)
	reg := newRegistry(t, "com.acme", "tap-attestation:v1")
	trustKey(t, reg)
	dir, _ := stageAttested(t, c, reg, nil)

	// The attestation is genuine and verified until the program is replaced.
	if got := searchLabel(t, c); got != "verified by tap-attestation:v1" {
		t.Fatalf("before the swap: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.sh"), []byte("echo swapped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	search := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
	match := search["matches"].([]any)[0].(map[string]any)
	if label, _ := match["attestation"].(string); !strings.HasPrefix(label, "attestation invalid") || !strings.Contains(label, "run digest") {
		t.Fatalf("after the swap: %q", label)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"ref": match["ref"], "digest": match["digest"]}})
	if run["isError"] != true || !strings.Contains(toolText(t, run), "refused") || strings.Contains(toolText(t, run), "swapped") {
		t.Fatalf("a package whose files are not the signed ones must be refused, and not run: %#v", run)
	}
}
