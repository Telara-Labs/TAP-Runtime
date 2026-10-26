package redact

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

const PushMetadataProfile = "push-metadata"

// Profiles returns independent storage. Reserved future profiles are not
// advertised until their behavior is implemented and fingerprinted.
func (Ruleset) Profiles() []string { return []string{"local", PushMetadataProfile} }

// RedactProfile applies the base floor and the named profile at every decoded
// key/value. Unknown profiles fail closed, including empty or future names.
func (Ruleset) RedactProfile(profile, text string) string {
	switch profile {
	case "local":
		return Redact(text)
	case PushMetadataProfile:
		return inspectProfile(text, false, true).text
	default:
		return "<redacted unsupported profile>"
	}
}

// Address matching is intentionally lexical, without DNS or network access.
// Only the exact reserved example.invalid domain and its subdomains are exempt
// from the email extra. Credential assignments and the frozen floor still win.
var metadataEmail = regexp.MustCompile("[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@(?:[A-Za-z0-9-]+\\.)+[A-Za-z]{2,63}")
var metadataIPv4 = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}`)
var metadataIPv6 = regexp.MustCompile(`[0-9A-Fa-f:.]+(?:%[A-Za-z0-9_.-]+)?`)
var metadataURL = regexp.MustCompile("(?i)\\bhttps?://[^\\s<>\\\"'`]+")
var metadataGroq = regexp.MustCompile(`gsk_[A-Za-z0-9]{48,}`)

func metadataSpans(s string) []valueSpan {
	var spans []valueSpan
	for _, m := range metadataEmail.FindAllStringIndex(s, -1) {
		address := s[m[0]:m[1]]
		domain := strings.ToLower(address[strings.LastIndexByte(address, '@')+1:])
		if domain != "example.invalid" && !strings.HasSuffix(domain, ".example.invalid") {
			spans = append(spans, valueSpan{m[0], m[1]})
		}
	}
	for _, re := range []*regexp.Regexp{metadataIPv4} {
		for _, m := range re.FindAllStringIndex(s, -1) {
			start, end := m[0], m[1]
			// Sentence punctuation is not part of an address. A dotted version,
			// hostname or identifier adjoining it is not an address literal.
			for end > start && s[end-1] == '.' {
				end--
			}
			if start > 0 && addressWord(s[start-1]) || end < len(s) && addressWord(s[end]) && !(s[end] == '.' && (end+1 == len(s) || !addressWord(s[end+1]))) {
				continue
			}
			if _, err := netip.ParseAddr(s[start:end]); err == nil {
				spans = append(spans, valueSpan{start, end})
			}
		}
	}
	spans = append(spans, metadataIPv6Spans(s)...)
	for _, m := range metadataURL.FindAllStringIndex(s, -1) {
		if secretWebhook(s[m[0]:m[1]]) {
			spans = append(spans, valueSpan{m[0], m[1]})
		}
	}
	for _, m := range metadataGroq.FindAllStringIndex(s, -1) {
		spans = append(spans, valueSpan{m[0], m[1]})
	}
	return spans
}

// An IPv6 literal is at most 45 bytes before its optional zone. Search bounded
// starts/ends rather than rejecting a maximal run containing adjacent colons.
// This also bounds work on hostile long colon runs without resolving names.
func metadataIPv6Spans(s string) []valueSpan {
	var spans []valueSpan
	for _, match := range metadataIPv6.FindAllStringIndex(s, -1) {
		begin, end := match[0], match[1]
		baseEnd := end
		if at := strings.IndexByte(s[begin:end], '%'); at >= 0 {
			baseEnd = begin + at
		}
		for start := begin; start < baseEnd; start++ {
			if start > 0 && addressWord(s[start-1]) {
				continue
			}
			for stop := min(baseEnd, start+45); stop > start+1; stop-- {
				last := stop
				if stop == baseEnd {
					last = end // Include an attached zone in the same removal.
				}
				addr, err := netip.ParseAddr(s[start:last])
				if err == nil && addr.Is6() {
					boundary := last
					// Sentence ellipses are punctuation, but a following word or
					// dotted identifier must still block an embedded match.
					for boundary < len(s) && s[boundary] == '.' {
						boundary++
					}
					if boundary < len(s) && addressWord(s[boundary]) {
						continue
					}
					spans = append(spans, valueSpan{start, last})
					break
				}
			}
		}
	}
	return spans
}

func addressWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}

func secretWebhook(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	path := strings.Trim(u.Path, "/") // url.Parse decodes path escapes.
	parts := strings.Split(path, "/")
	if (host == "hooks.slack.com" || host == "hooks.slack-gov.com") && len(parts) >= 4 && parts[0] == "services" {
		return true
	}
	if host == "discord.com" || host == "discordapp.com" || host == "canary.discord.com" || host == "ptb.discord.com" {
		for i, part := range parts {
			if part == "webhooks" && i+2 < len(parts) && parts[i+1] != "" && parts[i+2] != "" {
				return true
			}
		}
	}
	hook := strings.HasPrefix(host, "hooks.") || strings.HasPrefix(host, "webhooks.")
	pathHook := false
	for _, part := range parts {
		if pathHook && len(part) >= 8 && !variableReference(part) {
			return true
		}
		if part == "hooks" || part == "hook" || part == "webhooks" || part == "webhook" {
			hook, pathHook = true, true
		}
	}
	if !hook {
		return false
	}
	if (strings.HasPrefix(host, "hooks.") || strings.HasPrefix(host, "webhooks.")) && len(parts) > 0 && len(parts[len(parts)-1]) >= 8 && !variableReference(parts[len(parts)-1]) {
		return true
	}
	for name, values := range u.Query() {
		if IsSensitiveName(name) || name == "key" || name == "code" || name == "sig" || name == "signature" {
			for _, value := range values {
				if value != "" && !variableReference(value) {
					return true
				}
			}
		}
	}
	return false
}
