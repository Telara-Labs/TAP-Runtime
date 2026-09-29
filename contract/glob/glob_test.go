package glob

import "testing"

func TestArgs(t *testing.T) {
	for _, c := range []struct {
		pattern, args []string
		want          bool
	}{
		// The example of ruling 31.
		{[]string{"get", "pods", "*"}, []string{"get", "pods", "-n", "app"}, true},
		{[]string{"get", "pods", "*"}, []string{"get", "pods"}, true},
		{[]string{"get", "pods", "*"}, []string{"get", "secrets"}, false},
		{[]string{"get", "pods", "*"}, []string{"get"}, false},
		// Without a final *, the count is exact.
		{[]string{"get", "pods"}, []string{"get", "pods"}, true},
		{[]string{"get", "pods"}, []string{"get", "pods", "-o", "yaml"}, false},
		// A * that is not last is one word, not the rest.
		{[]string{"get", "*", "-n", "app"}, []string{"get", "pods", "-n", "app"}, true},
		{[]string{"get", "*", "-n", "app"}, []string{"get", "pods", "extra", "-n", "app"}, false},
		// A word may be part wildcard.
		{[]string{"log", "--since=*"}, []string{"log", "--since=2 days"}, true},
		{[]string{"log", "v[0-9]*"}, []string{"log", "v12"}, true},
		{[]string{"log", "v[0-9]*"}, []string{"log", "main"}, false},
		{[]string{"rollout", "status", "deploy/?"}, []string{"rollout", "status", "deploy/a"}, true},
		{[]string{"rollout", "status", "deploy/?"}, []string{"rollout", "status", "deploy/ab"}, false},
		// An argument is one word whatever it contains.
		{[]string{"cat", "*"}, []string{"cat", "/etc/passwd"}, true},
		{[]string{"*"}, nil, true},
		{[]string{}, nil, true},
		{[]string{}, []string{"anything"}, false},
	} {
		if got := Args(c.pattern, c.args); got != c.want {
			t.Errorf("Args(%q, %q) = %v, want %v", c.pattern, c.args, got, c.want)
		}
	}
}

func TestWord(t *testing.T) {
	for _, c := range []struct {
		pattern, word string
		want          bool
	}{
		{"AWS_*", "AWS_REGION", true},
		{"AWS_*", "AWS_", true},
		{"AWS_*", "XAWS_REGION", false},
		{"AWS_*", "aws_region", false},
		{"KUBECONFIG", "KUBECONFIG", true},
		{"KUBECONFIG", "KUBECONFIG2", false},
		{"*_TOKEN", "GITLAB_TOKEN", true},
		{"[!A]*", "BWS", true},
		{"[!A]*", "AWS", false},
		{"a[", "a[", true},
	} {
		if got := Word(c.pattern, c.word); got != c.want {
			t.Errorf("Word(%q, %q) = %v, want %v", c.pattern, c.word, got, c.want)
		}
	}
}

func TestPath(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"reports/*.txt", "reports/a.txt", true},
		{"reports/*.txt", "reports/a.md", false},
		// * does not cross a directory.
		{"reports/*.txt", "reports/2026/a.txt", false},
		{"reports/*", "reports/2026/a.txt", false},
		// ** does.
		{"reports/**", "reports/2026/q3/a.txt", true},
		{"reports/**", "reports", true},
		{"reports/**/*.txt", "reports/a.txt", true},
		{"reports/**/*.txt", "reports/2026/q3/a.txt", true},
		{"reports/**/*.txt", "reports/2026/q3/a.md", false},
		{"reports/**", "reports-private/a.txt", false},
		{"**/secrets/*", "a/b/secrets/k", true},
		{"data/?.csv", "data/a.csv", true},
		{"data/?.csv", "data/ab.csv", false},
	} {
		if got := Path(c.pattern, c.path); got != c.want {
			t.Errorf("Path(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestHost(t *testing.T) {
	for _, c := range []struct {
		pattern, host string
		want          bool
	}{
		{"*.atlassian.net", "telara.atlassian.net", true},
		{"*.atlassian.net", "TELARA.Atlassian.NET", true},
		// One level, and only one.
		{"*.atlassian.net", "atlassian.net", false},
		{"*.atlassian.net", "a.b.atlassian.net", false},
		{"*.atlassian.net", "evilatlassian.net", false},
		{"*.atlassian.net", "atlassian.net.evil.com", false},
		{"api.github.com", "api.github.com", true},
		{"api.github.com", "uploads.github.com", false},
		// A wildcard anywhere else is not a wildcard.
		{"api.*.com", "api.github.com", false},
	} {
		if got := Host(c.pattern, c.host); got != c.want {
			t.Errorf("Host(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}
