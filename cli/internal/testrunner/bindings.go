package testrunner

import (
	"fmt"
	"net/url"
	"strings"

	"telara.dev/tap/internal/model"
)

// ParseBindFlags parses repeated `--bind slot=origin` values (DECISION:
// bind-simulation via CLI flag, see README/final report -- this is what
// makes web-changelog-watch's origin_blocked contract case testable
// offline without a real platform bind).
func ParseBindFlags(vals []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range vals {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--bind must be slot=origin, got %q", v)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}

// MergeBindings merges CLI-flag bindings over a contract file's optional
// tests-local `bindings:` block (flag wins on conflict).
func MergeBindings(fileBindings, flagBindings map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range fileBindings {
		out[k] = v
	}
	for k, v := range flagBindings {
		out[k] = v
	}
	return out
}

// sameOrigin compares scheme+host only (path/query irrelevant to an origin
// check).
func sameOrigin(a, b string) (bool, error) {
	ua, err := url.Parse(a)
	if err != nil {
		return false, err
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host), nil
}

// originOf returns "scheme://host" for a URL string.
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// hostOf extracts a bare hostname from either a plain hostname string or a
// full URL (egress-slot binds may be given either way, e.g.
// `--bind jira_site=https://acme-corp.atlassian.net` or
// `--bind jira_site=acme-corp.atlassian.net`).
func hostOf(raw string) string {
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return raw
}

// egressHostAllowed reports whether host is covered by the manifest's
// declared requirements.network.egressHosts, per CHANGELOG.md v1 ruling 2
// (egress-host slots): a literal entry matches by exact (case-insensitive)
// hostname; a `{slot: name}` entry matches only when that slot is bound in
// this run AND the bound host equals host -- an unbound egress slot
// contributes no match (unlike the browser-origin slot's "unbound = skip
// the check" offline-testing semantics: egress refusal is only ever
// evaluated for a test's explicit `override_step.egress_host`, a deliberate
// opt-in negative-test mechanism, so defaulting an unbound slot to "allow"
// here would silently defeat the very negative case it's meant to prove).
func egressHostAllowed(hosts []model.EgressHostEntry, host string, binds map[string]string) bool {
	for _, h := range hosts {
		if h.Literal != "" && strings.EqualFold(h.Literal, host) {
			return true
		}
		if h.Slot != "" {
			if bound, ok := binds[h.Slot]; ok && strings.EqualFold(hostOf(bound), host) {
				return true
			}
		}
	}
	return false
}
