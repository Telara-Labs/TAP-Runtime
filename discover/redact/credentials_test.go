package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every value here is synthetic and deliberately unusable.
func TestRedactRemovesWholePrivateKeyBlocks(t *testing.T) {
	for _, kind := range []string{"", "RSA ", "EC ", "OPENSSH ", "ENCRYPTED "} {
		for _, separator := range []string{"\n", "\r\n", `\n`} {
			block := "-----BEGIN " + kind + "PRIVATE KEY-----" + separator + "SYNTHETIC-KEY-BODY" + separator + "-----END " + kind + "PRIVATE KEY-----"
			got := Redact("before " + block + " after")
			if strings.Contains(got, "SYNTHETIC-KEY-BODY") || strings.Contains(got, "PRIVATE KEY-----") || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
				t.Fatalf("key block not fully removed: %q", got)
			}
		}
	}
	if got := Redact("before -----BEGIN RSA PRIVATE KEY-----\nSYNTHETIC-TRUNCATED-BODY"); strings.Contains(got, "SYNTHETIC-TRUNCATED-BODY") || !strings.Contains(got, "before") {
		t.Fatalf("truncated key body retained: %q", got)
	}
	public := "-----BEGIN PUBLIC KEY-----\nSYNTHETIC-PUBLIC\n-----END PUBLIC KEY-----"
	if Redact(public) != public {
		t.Fatal("a public key was removed")
	}
}

func TestArgumentRedactsOpaqueValuesByFieldName(t *testing.T) {
	for _, name := range []string{"password", "token", "apiKey", "API_TOKEN", "client-secret", "credentials", "--password=", "Authorization", "AWS_SECRET_ACCESS_KEY", "secret_key", "SERVICE_SECRET_KEY"} {
		if got := Argument(name, "synthetic-short"); strings.Contains(got, "synthetic-short") || !strings.Contains(got, "redacted") {
			t.Fatalf("%s retained its credential: %q", name, got)
		}
		for _, ref := range []string{"$SYNTHETIC_TOKEN", "${SYNTHETIC_TOKEN}", "{env:SYNTHETIC_TOKEN}", ""} {
			if got := Argument(name, ref); got != ref {
				t.Fatalf("%s reference changed: %q -> %q", name, ref, got)
			}
		}
	}
	for name, value := range map[string]string{"issue_key": "ABC-12", "max_output_tokens": "4000", "model": "ordinary-model", "code": "git status"} {
		if got := Argument(name, value); got != value {
			t.Fatalf("ordinary argument changed: %s %q -> %q", name, value, got)
		}
	}
	if got := Argument("token", "${SYNTHETIC_TOKEN:-synthetic-default}"); strings.Contains(got, "synthetic-default") {
		t.Fatal("a literal fallback was mistaken for a variable reference")
	}
}

func TestArgumentRedactsNestedCredentialsAndPreservesConfig(t *testing.T) {
	in := `{"env":{"API_TOKEN":"synthetic-opaque","MODE":"development","PASSWORD":"x","REFRESH_TOKEN":"${SYNTHETIC_TOKEN}"},"rows":[{"credentials":{"value":"synthetic-hidden"},"issue_key":"ABC-12"}],"max_output_tokens":9007199254740993,"enabled":true,"vars":["PASSWORD=short","MODE=development","API_TOKEN=$SYNTHETIC_TOKEN"],"nested":"{\"password\":\"synthetic-nested\",\"limit\":5}"}`
	got := Argument("config", in)
	for _, value := range []string{"synthetic-opaque", `"PASSWORD":"x"`, "synthetic-hidden", "PASSWORD=short", "synthetic-nested"} {
		if strings.Contains(got, value) {
			t.Fatalf("nested credential retained: %s in %s", value, got)
		}
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("redacted structure is not JSON: %s", got)
	}
	for _, value := range []string{"development", "ABC-12", "9007199254740993", `"enabled":true`, "${SYNTHETIC_TOKEN}", "API_TOKEN=$SYNTHETIC_TOKEN"} {
		if !strings.Contains(got, value) {
			t.Fatalf("ordinary configuration lost: %s in %s", value, got)
		}
	}
	// JSON payloads printed as request/output text need the same field context.
	if got := Redact(`{"password":"x","nested":{"token":1234},"count":5}`); strings.Contains(got, `"x"`) || strings.Contains(got, "1234") || !strings.Contains(got, `"count":5`) || !json.Valid([]byte(got)) {
		t.Fatalf("structured output not safely redacted: %s", got)
	}
}

func BenchmarkArgumentOrdinaryText(b *testing.B) {
	for b.Loop() {
		Argument("issue_key", "ABC-12")
	}
}

func TestRedactShellEnvironmentKeepsTheProcedure(t *testing.T) {
	in := `API_TOKEN=x PASSWORD='synthetic phrase' REGION=west PATH=/ordinary/bin tool --issue_key ABC-12; TOKEN="$SYNTHETIC_TOKEN" OTHER_TOKEN=${SYNTHETIC_TOKEN} MODEL_TOKEN='{env:SYNTHETIC_TOKEN}' tool status`
	got := Redact(in)
	for _, secret := range []string{"API_TOKEN=x", "synthetic phrase"} {
		if strings.Contains(got, secret) {
			t.Fatalf("env credential retained: %s", got)
		}
	}
	for _, normal := range []string{"REGION=west", "PATH=/ordinary/bin", "tool --issue_key ABC-12", `TOKEN="$SYNTHETIC_TOKEN"`, "OTHER_TOKEN=${SYNTHETIC_TOKEN}", "MODEL_TOKEN='{env:SYNTHETIC_TOKEN}'", "tool status"} {
		if !strings.Contains(got, normal) {
			t.Fatalf("normal command context lost: %s in %s", normal, got)
		}
	}
}

func TestRedactPreservesBenignLargeJSONBytes(t *testing.T) {
	in := "{\n  \"rows\": [" + strings.Repeat(`{"issue_key":"ABC-12", "region":"west", "count":9007199254740993},`, 1000) + `{"path":"/ordinary/bin"}], "enabled":true}`
	if got := Redact(in); got != in {
		t.Fatal("benign JSON was reformatted or changed")
	}
	plain := strings.Repeat("ordinary output and public identifiers\n", 1000)
	if got := Redact(plain); got != plain {
		t.Fatal("benign text output changed")
	}
}

func BenchmarkRedactBenignJSON(b *testing.B) {
	in := benignJSONBenchmark()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		Redact(in)
	}
}

func BenchmarkRedactBenignJSONTextPath(b *testing.B) {
	in := benignJSONBenchmark()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		redactText(in)
	}
}

func BenchmarkRedactBenignJSONLegacy(b *testing.B) {
	in := benignJSONBenchmark()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		out := in
		for _, sh := range SecretShapes {
			out = sh.re.ReplaceAllString(out, "<redacted "+sh.name+">")
		}
	}
}

func benignJSONBenchmark() string {
	return `{"rows":[` + strings.Repeat(`{"issue_key":"ABC-12","region":"west"},`, 100) + `{"limit":5}]}`
}

func TestStructuredRedactionDecodesEscapedSecretNames(t *testing.T) {
	in := `{"\u0070assword":"synthetic-escaped-value","issue_key":"ABC-12"}`
	got := Redact(in)
	if strings.Contains(got, "synthetic-escaped-value") || !strings.Contains(got, "ABC-12") || !json.Valid([]byte(got)) {
		t.Fatalf("escaped sensitive name retained: %s", got)
	}
	in = `{"value":"` + fakeGitlab + `","count":5}`
	got = Redact(in)
	if strings.Contains(got, fakeGitlab) || !strings.Contains(got, `"count":5`) || !json.Valid([]byte(got)) {
		t.Fatalf("value-shaped credential reached structured output: %s", got)
	}
}
