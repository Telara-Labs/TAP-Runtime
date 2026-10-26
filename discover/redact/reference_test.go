package redact

// Frozen unoptimized inspection procedures retained only as a differential
// oracle. These methods never call the fast inspector. Shared pattern/context
// helpers remain the versioned ruleset, whose behavior golden is independent.
import (
	"encoding/json"
	"github.com/Telara-Labs/TAP-Runtime/contract/canon"
	"gopkg.in/yaml.v3"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type referenceInspector struct {
	remaining                   int
	schemaData, metadataProfile bool
}

func (p *referenceInspector) text(s string, sensitive bool, depth int) inspection {
	if depth > 64 || len(s) > p.remaining {
		return inspection{"<redacted inspection limit>", "inspection limit"}
	}
	// All original/pristine/current frozen spans contribute to the floor.
	// Protect spans extending beyond current normalization BEFORE parsing;
	// current-view spans remain independently enforced by text and gatewayFloor.
	// Never replace only a prefix of an original frozen value.
	spans := referenceFrozenSourceSpans(s)
	current := referenceNormalizedFloorSpans(s, referenceNormalizeRune)
	// Sort and scan coverage once; hostile repeated assignments must not
	// turn the independent floor into a quadratic comparison.
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	sort.Slice(current, func(i, j int) bool { return current[i].start < current[j].start })
	var extending []valueSpan
	at, coveredEnd := 0, -1
	for _, span := range spans {
		for at < len(current) && current[at].start <= span.start {
			coveredEnd = max(coveredEnd, current[at].end)
			at++
		}
		if coveredEnd < span.end {
			extending = append(extending, span)
		}
	}
	// A frozen prefix must not erase a normalized PEM header while leaving
	// its multiline body. Add every intersecting complete/incomplete PEM span
	// to the same source union before any replacement happens.
	if len(extending) > 0 {
		prefixEnd := make([]int, len(extending))
		end := 0
		for i, span := range extending {
			end = max(end, span.end)
			prefixEnd[i] = end
		}
		var blocks []valueSpan
		for _, shape := range SecretShapes {
			if shape.name != "private key" {
				continue
			}
			blocks = referenceProjectedSpans(s, referenceNormalizeRune, func(view string) []valueSpan {
				var found []valueSpan
				for _, m := range shape.re.FindAllStringIndex(view, -1) {
					found = append(found, valueSpan{m[0], m[1]})
				}
				return found
			})
		}
		originalExtending := extending
		for _, block := range blocks {
			i := sort.Search(len(originalExtending), func(i int) bool { return originalExtending[i].start >= block.end })
			if i > 0 && prefixEnd[i-1] > block.start {
				extending = append(extending, block)
			}
		}
	}
	marker := inspectionMarker(s, "redactedcredential")
	protected := replaceSpanUnion(s, extending, marker)
	if json.Valid([]byte(s)) && !json.Valid([]byte(protected)) {
		protected = encodeValue("<redacted credential>")
	}
	result := referenceGatewayFloor(s, p.textView(protected, sensitive, depth))
	result.text = strings.ReplaceAll(result.text, marker, "<redacted credential>")
	if len(spans) > 0 {
		result.shape = detectionClass(referenceNormalize(s), "credential")
	}
	return result
}

func referenceFrozenSourceSpans(source string) []valueSpan {
	spans := referenceGatewayCredentialSpans(source)
	spans = append(spans, referenceNormalizedFloorSpans(source, referencePristineFloorRune)...)
	return append(spans, referenceNormalizedFloorSpans(source, referenceNormalizeRune)...)
}

func referenceNormalizedFloorSpans(source string, normalize func(rune) rune) []valueSpan {
	return referenceProjectedSpans(source, normalize, referenceGatewayCredentialSpans)
}

func (p *referenceInspector) textView(original string, sensitive bool, depth int) (result inspection) {
	rawMarker := ""
	defer func() {
		if rawMarker != "" {
			result.text = strings.ReplaceAll(result.text, rawMarker, "<redacted credential>")
		}
	}()
	if depth > 64 || len(original) > p.remaining {
		return inspection{"<redacted inspection limit>", "inspection limit"}
	}
	p.remaining -= len(original)
	// Decode original values before normalization. Detection views may change;
	// unrelated decoded siblings must retain their exact contents.
	if value, ok := structured(original); ok {
		result := p.json(original, value, sensitive, depth)
		if result.shape == "" {
			result.text = original
		}
		return result
	}
	s := referenceNormalize(original)
	if sensitive && s != "" && !variableReference(s) {
		return inspection{"<redacted credential>", "credential"}
	}
	if !sensitive && !p.schemaData && strings.Contains(s, ":") {
		if result, ok := p.yamlSchema(referenceAssembleLine(s), depth); ok {
			result = referenceGatewayFloor(referenceAssembleLine(s), result)
			if result.shape == "" {
				result.text = original
			}
			return result
		}
	}
	// Compute independent raw-text spans before fragment protection. No later
	// parser can hide a frozen credential match. YAML contributes complete
	// scalar spans to the same union, including all physical continuation lines.
	logical := referenceAssembleLine(s)
	rawSpans := append(referenceGatewayCredentialSpans(logical), yamlCredentialSpans(logical)...)
	rawSpans = append(rawSpans, yamlFlowCredentialSpans(logical)...)
	for _, sh := range SecretShapes {
		if sh.name == "private key" {
			for _, m := range sh.re.FindAllStringIndex(logical, -1) {
				rawSpans = append(rawSpans, valueSpan{m[0], m[1]})
			}
		}
	}
	if len(rawSpans) > 0 {
		rawMarker = inspectionMarker(logical, "redactedcredential")
	}
	raw := replaceSpanUnion(logical, rawSpans, rawMarker)
	rawShape := ""
	if raw != logical {
		rawShape = "credential"
		s = raw
	}
	// Decode complete JSON fragments before text patterns. Every descendant
	// is inspected before an outer fragment is consumed; failed outer parses
	// advance only one byte so inner fragments cannot disappear from view.
	s, shape, restore := p.fragments(s, depth, rawMarker)
	if p.metadataProfile {
		if spans := metadataSpans(s); len(spans) > 0 {
			s = replaceSpanUnion(s, spans, "<redacted metadata>")
			shape = "metadata"
		}
	}
	if rawShape != "" {
		shape = rawShape
	}
	for _, sh := range SecretShapes {
		if sh.name == "private key" && sh.re.MatchString(s) {
			s = sh.re.ReplaceAllString(s, "<redacted private key>")
			shape = sh.name
		}
	}
	// Assemble before detection, including continuations inside quotes. The
	// resulting replacement covers all pieces of a raw continued credential.
	// Assignment parsing inspects multiline scalars without consuming mapping metadata.
	line := referenceAssembleLine(s)
	if line != s {
		// A continuation can complete JSON syntax too. Re-enter the same
		// decoder so encoded names/tokens cannot hide behind that composition.
		r := p.text(line, false, depth+1)
		if r.shape != "" {
			r.text = restore(r.text)
			return r
		}
	}
	next := redactQuotedFields(line, IsSensitiveName, restore)
	for _, sh := range SecretShapes {
		if sh.name == "private key" {
			next = sh.re.ReplaceAllString(next, "<redacted private key>")
		}
	}
	next = replaceValues(next, credentialAssignments(next, restore))
	next = redactShapes(next)
	if next != line {
		if shape == "" {
			shape = "credential"
		}
		s = next
	}
	if shape == "" {
		return inspection{original, ""}
	}
	return inspection{restore(s), detectionClass(referenceAssembleLine(referenceNormalize(original)), shape)}
}

func (p *referenceInspector) json(raw string, value any, sensitive bool, depth int) inspection {
	// A map decoder alone would discard shadowed values. Refuse duplicate
	// keys and other ambiguous encodings for whole roots and prose fragments.
	if _, err := canon.CanonicalJSON([]byte(raw)); err != nil {
		return inspection{"<redacted ambiguous JSON>", "ambiguous JSON"}
	}
	next, shape := p.valueContext(value, schemaContext{sensitive: sensitive, data: p.schemaData}, depth+1)
	if shape == "" {
		return inspection{raw, ""}
	}
	return inspection{encodeValue(next), shape}
}

func (p *referenceInspector) value(value any, sensitive bool, depth int) (any, string) {
	return p.valueContext(value, schemaContext{sensitive: sensitive, data: p.schemaData}, depth)
}

func (p *referenceInspector) valueContext(value any, context schemaContext, depth int) (any, string) {
	if depth > 64 {
		return "<redacted inspection limit>", "inspection limit"
	}
	shape := ""
	switch v := value.(type) {
	case map[string]any:
		context = context.mapping().discovery(value)
		for key := range v {
			context = context.recognize(key)
		}
		// Sort traversal so the reported reason and resource accounting are
		// deterministic even when several fields carry credentials.
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if r := p.text(key, false, depth); r.shape != "" {
				delete(v, key)
				shape = r.shape
				continue
			}
			child := context.childValue(key, v[key])
			beforeData := p.schemaData
			p.schemaData = child.data
			next, found := p.valueContext(v[key], child, depth+1)
			p.schemaData = beforeData
			v[key] = next
			if found != "" {
				shape = "secret in JSON"
			}
		}
	case []any:
		if context.dependency {
			if metadataStrings(value, false) {
				context = schemaContext{metadata: true}
			} else {
				context.data = true
				context.dependency = false
			}
		}
		for i, child := range v {
			next, found := p.valueContext(child, context, depth+1)
			v[i] = next
			if found != "" {
				shape = found
			}
		}
	case string:
		r := p.text(v, context.scalarSensitive(false), depth)
		return r.text, r.shape
	case nil:
	case bool:
		if context.scalarSensitive(true) {
			return "<redacted credential>", "credential"
		}
	default:
		if context.scalarSensitive(false) {
			return "<redacted credential>", "credential"
		}
	}
	return value, shape
}

func (p *referenceInspector) fragments(s string, depth int, rawMarker string) (string, string, func(string) string) {
	composites, shape, restoreComposites := p.fragmentPass(s, depth, fragmentComposites, rawMarker)
	quoted, found, _ := p.fragmentPass(composites, depth, fragmentQuotedCandidates, rawMarker)
	if found != "" {
		shape = found
	}
	protected, found, restoreQuoted := p.fragmentPass(quoted, depth, fragmentQuotedProtection, rawMarker)
	if found != "" {
		shape = found
	}
	return protected, shape, func(s string) string { return restoreComposites(restoreQuoted(s)) }
}

func (p *referenceInspector) fragmentPass(s string, depth, phase int, rawMarker string) (string, string, func(string) string) {
	var out strings.Builder
	last, shape := 0, ""
	skipClosing := make(map[int]bool)
	// Protect every fully inspected fragment from later raw-text patterns.
	// Otherwise a closing source-code quote can become a fictitious opening
	// assignment quote. Tokens are collision-free and cannot carry secrets.
	prefix := inspectionMarker(s, "fragmentplaceholder")
	var replacements []string
	var changes []fragmentChange
	record := func(start, end int, text, found string) {
		if phase == fragmentQuotedCandidates {
			if found == "" {
				return
			}
			if n := len(changes); n > 0 && start < changes[n-1].end {
				changes[n-1].end = max(changes[n-1].end, end)
				changes[n-1].text = "<redacted credential>"
			} else {
				changes = append(changes, fragmentChange{start, end, text})
			}
			return
		}
		token := prefix + "fragment00000000" + strconv.Itoa(len(replacements)/2) + prefix
		out.WriteString(s[last:start])
		out.WriteString(token)
		replacements = append(replacements, token, text)
		last = end
	}
	finish := func(masked, shape string) (string, string, func(string) string) {
		if phase == fragmentQuotedCandidates {
			var result strings.Builder
			last := 0
			for _, change := range changes {
				result.WriteString(s[last:change.start])
				result.WriteString(change.text)
				last = change.end
			}
			result.WriteString(s[last:])
			return result.String(), shape, func(s string) string { return s }
		}
		if len(replacements) == 0 {
			return masked, shape, func(s string) string { return s }
		}
		return masked, shape, strings.NewReplacer(replacements...).Replace
	}
	for i := 0; i < len(s); i++ {
		if phase == fragmentComposites && s[i] != '{' && s[i] != '[' {
			continue
		}
		if phase != fragmentComposites && s[i] != '"' && s[i] != '\'' {
			continue
		}
		if s[i] == '\'' {
			// Shell/source single-quoted strings are literal spans too. Inspect
			// their contents before protecting their quote boundaries.
			close := strings.IndexByte(s[i+1:], '\'')
			if close < 0 {
				continue
			}
			end := i + close + 2
			inner := s[i+1 : end-1]
			r := p.text(inner, false, depth+1)
			following := end
			for following < len(s) && asciiSpace(s[following]) {
				following++
			}
			key := following < len(s) && (s[following] == ':' || s[following] == '=')
			if r.shape == "" && (variableReference(inner) || key) {
				if phase != fragmentQuotedCandidates {
					i = end - 1
				}
				continue
			}
			if r.shape != "" {
				shape = r.shape
			}
			record(i, end, "'"+r.text+"'", r.shape)
			if phase != fragmentQuotedCandidates {
				i = end - 1
			}
			continue
		}
		if s[i] != '{' && s[i] != '[' && s[i] != '"' {
			continue
		}
		if phase != fragmentQuotedCandidates && skipClosing[i] {
			continue
		}
		// Bound attempted decoding by work remaining. Small successful
		// fragments and syntax errors are charged by the consumed prefix.
		available := min(len(s)-i, p.remaining)
		if available <= 0 {
			record(i, len(s), "<redacted inspection limit>", "inspection limit")
			out.WriteString(s[last:])
			return finish(out.String(), "inspection limit")
		}
		d := json.NewDecoder(strings.NewReader(s[i : i+available]))
		d.UseNumber()
		var value any
		if err := d.Decode(&value); err != nil {
			if s[i] == '"' {
				// A malformed string's closing quote must not consume the
				// colon/value boundary as a new standalone string. Leave the
				// complete raw field available to the tolerant field parser.
				for close := i + 1; close < i+available; close++ {
					if s[close] == '\\' {
						close++
						continue
					}
					if s[close] == '"' {
						skipClosing[close] = true
						break
					}
				}
			}
			cost := available
			if syntax, ok := err.(*json.SyntaxError); ok {
				cost = int(syntax.Offset)
			}
			p.remaining -= cost
			if available < len(s)-i && cost == available {
				record(i, len(s), "<redacted inspection limit>", "inspection limit")
				out.WriteString(s[last:])
				return finish(out.String(), "inspection limit")
			}
			continue
		}
		p.remaining -= int(d.InputOffset())
		end := i + int(d.InputOffset())
		raw := s[i:end]
		field, keySyntax := false, false
		if _, isString := value.(string); isString {
			// A quoted key/value pair outside braces is a field fragment too.
			// Decode its complete value before shielding the key, including
			// nested objects and arrays rather than swallowing their contents.
			colon := end
			for colon < len(s) && asciiSpace(s[colon]) {
				colon++
			}
			keySyntax = colon < len(s) && (s[colon] == ':' || s[colon] == '=')
			if colon < len(s) && s[colon] == ':' {
				available := min(len(s)-colon-1, p.remaining)
				d := json.NewDecoder(strings.NewReader(s[colon+1 : colon+1+available]))
				d.UseNumber()
				var child any
				if err := d.Decode(&child); err == nil {
					p.remaining -= int(d.InputOffset())
					childEnd := colon + 1 + int(d.InputOffset())
					following := childEnd
					for following < len(s) && asciiSpace(s[following]) {
						following++
					}
					_, childString := child.(string)
					if childString && following < len(s) && s[following] == ':' {
						// The decoded string is a nested YAML mapping key,
						// rather than the outer field's credential scalar.
						keySyntax = true
					} else {
						end = childEnd
						raw = "{" + s[i:end] + "}"
						value = map[string]any{value.(string): child}
						field = true
					}
				} else {
					cost := available
					if syntax, ok := err.(*json.SyntaxError); ok {
						cost = int(syntax.Offset)
					}
					p.remaining -= cost
				}
			}
		}
		r := p.json(raw, value, false, depth)
		// A compatibility-layer replacement is a real edit even if the
		// decoded value no longer has a provider shape. Serialize the decoded
		// fragment so downstream view filters still see reconstructed syntax.
		if r.shape == "" && rawMarker != "" && strings.Contains(raw, rawMarker) {
			r = inspection{encodeValue(value), "credential"}
		}
		if r.shape != "" {
			shape = r.shape
		}
		if field {
			if r.shape == "" {
				r.text = s[i:end]
			} else if strings.HasPrefix(r.text, "{") && strings.HasSuffix(r.text, "}") {
				r.text = r.text[1 : len(r.text)-1]
			}
		}
		if ref, ok := value.(string); ok && r.shape == "" && (variableReference(ref) || keySyntax && !field) {
			// Keep complete references visible to shell assignment parsing.
			if phase != fragmentQuotedCandidates {
				i = end - 1
			}
			continue
		}
		record(i, end, r.text, r.shape)
		if phase != fragmentQuotedCandidates {
			i = end - 1
		}
	}
	out.WriteString(s[last:])
	return finish(out.String(), shape)
}

func referenceAssembleLine(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\n' {
				i++
				continue
			}
			if s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n' {
				i += 2
				continue
			}
			out.WriteByte(s[i])
			i++
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func referenceGatewayCredentialSpans(s string) []valueSpan {
	var spans []valueSpan
	for i, re := range gatewayCredentialPatterns {
		for _, m := range re.FindAllStringIndex(s, -1) {
			start, end := m[0], m[1]
			if i < 2 {
				at := strings.IndexAny(s[start:end], ":=") + start + 1
				for at < len(s) && asciiSpace(s[at]) {
					at++
				}
				if at < end && (s[at] == '"' || s[at] == '\'') {
					at++
				}

				start = at
			}
			spans = append(spans, valueSpan{start, end})
		}
	}
	return spans
}

func referenceGatewayFloor(source string, result inspection) inspection {
	output := referenceNormalize(result.text)
	var remaining []valueSpan
	budget := 64*len(source) + 65536
	checked := make(map[string]bool)
	values := make([]string, 0)
	for _, span := range referenceFrozenSourceSpans(source) {
		value := source[span.start:span.end]
		for depth := 0; ; depth++ {
			normalized := referenceNormalize(value)
			if checked[normalized] {
				break
			}
			checked[normalized] = true
			values = append(values, normalized)
			// Decoding can turn an originally contiguous frozen value into
			// delimiter-separated pieces. Structured inspection may already
			// remove one piece; retain coverage of every remaining piece too.
			// This only adds decisions from the original frozen value.
			if pieces := strings.FieldsFunc(normalized, func(r rune) bool {
				return unicode.IsSpace(r) || r == ',' || r == '}' || r == '"' || r == '\''
			}); len(pieces) > 1 {
				for _, piece := range pieces {
					if !checked[piece] {
						checked[piece] = true
						values = append(values, piece)
					}
				}
			}
			// Structured inspection can decode and reserialize a different field,
			// changing this frozen value's escape spelling without removing it.
			// Keep its decoded representations in the unconditional floor too.
			if !strings.Contains(value, `\`) {
				break
			}
			if depth >= 64 || len(value) > budget {
				return inspection{"<redacted inspection limit>", "inspection limit"}
			}
			budget -= len(value)
			var decoded string
			if json.Unmarshal([]byte(`"`+value+`"`), &decoded) != nil || decoded == "" || decoded == value {
				break
			}
			value = decoded
		}
	}
	for _, value := range values {
		if len(output) > budget {
			return inspection{"<redacted inspection limit>", "inspection limit"}
		}
		budget -= len(output)
		for at := 0; at < len(output); {
			found := strings.Index(output[at:], value)
			if found < 0 {
				break
			}
			start := at + found
			end := start + len(value)
			remaining = append(remaining, valueSpan{start, end})
			if len(remaining) > 4096 {
				return inspection{"<redacted inspection limit>", "inspection limit"}
			}
			at = end
		}
	}
	if len(remaining) == 0 {
		return result
	}
	projected := referenceProjectedSpans(result.text, referenceNormalizeRune, func(string) []valueSpan { return remaining })
	next := replaceSpanUnion(result.text, projected, "<redacted credential>")
	// A residual numeric/container span can cross JSON token boundaries. Keep
	// valid JSON valid even if the structured parser failed to remove that value.
	if json.Valid([]byte(result.text)) && !json.Valid([]byte(next)) {
		next = encodeValue("<redacted credential>")
	}
	return inspection{next, "credential"}
}

func (p *referenceInspector) yamlSchema(s string, depth int) (inspection, bool) {
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(s))
	if decoder.Decode(&document) != nil || len(document.Content) != 1 {
		return inspection{}, false
	}
	var trailing yaml.Node
	if decoder.Decode(&trailing) != io.EOF {
		return inspection{}, false
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode && root.Kind != yaml.SequenceNode {
		return inspection{}, false
	}
	seen, invalid := false, false
	shape := ""
	var walk func(*yaml.Node, schemaContext, int)
	walk = func(n *yaml.Node, c schemaContext, d int) {
		if d > 64 || p.remaining < 1 {
			invalid = true
			return
		}
		p.remaining--
		if n.Anchor != "" || n.Kind == yaml.AliasNode {
			// Treat identifiers as text too. Conservative refusal prevents both
			// alias-context leaks and restoration of an already redacted scalar.
			name := n.Anchor
			if n.Kind == yaml.AliasNode {
				name = n.Value
			}
			if r := p.text(name, false, d+1); r.shape != "" {
				shape = r.shape
			}
			invalid = true
			if n.Kind == yaml.AliasNode {
				return // Never follow n.Alias, including cyclic graphs.
			}
		}
		if n.Tag != "" && n.Tag != "!!map" && n.Tag != "!!seq" && n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!float" && n.Tag != "!!bool" && n.Tag != "!!null" && n.Tag != "!!timestamp" {
			invalid = true
		}
		for _, comment := range []*string{&n.HeadComment, &n.LineComment, &n.FootComment} {
			if *comment != "" {
				r := p.text(*comment, false, d+1)
				if r.shape != "" {
					*comment = r.text
					shape = r.shape
				}
			}
		}
		switch n.Kind {
		case yaml.MappingNode:
			c = c.mapping().discovery(n)
			for i := 0; i+1 < len(n.Content); i += 2 {
				c = c.recognize(n.Content[i].Value)
			}
			seen = seen || c.schema || c.properties || c.oauthFlow || c.oauthScopes || c.oauthResource || c.oauthIssuer || c.sensitive
			keys := make(map[string]bool)
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || keys[key.Value] || key.Tag != "!!str" {
					invalid = true
					continue
				}
				keys[key.Value] = true
				name := key.Value
				walk(key, schemaContext{metadata: true}, d+1)
				walk(n.Content[i+1], c.childValue(name, n.Content[i+1]), d+1)
			}
		case yaml.DocumentNode, yaml.SequenceNode:
			if n.Kind == yaml.SequenceNode && c.dependency {
				if metadataStrings(n, false) {
					c = schemaContext{metadata: true}
				} else {
					c.data = true
					c.dependency = false
				}
			}
			for _, child := range n.Content {
				walk(child, c, d+1)
			}
		case yaml.ScalarNode:
			before := p.schemaData
			p.schemaData = c.data
			r := p.text(n.Value, c.scalarSensitive(n.Tag == "!!bool"), d+1)
			p.schemaData = before
			if r.shape != "" {
				n.Value = r.text
				n.Tag = "!!str"
				n.Style = yaml.DoubleQuotedStyle
				shape = r.shape
			}
		default:
			invalid = true
		}
	}
	walk(&document, schemaContext{}, depth+1)
	if !seen {
		return inspection{}, false
	}
	if invalid {
		return inspection{"<redacted ambiguous YAML>", "ambiguous YAML"}, true
	}
	if shape == "" {
		return inspection{s, ""}, true
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return inspection{"<redacted ambiguous YAML>", "ambiguous YAML"}, true
	}
	return inspection{string(encoded), shape}, true
}

// The oracle's normalization, rune projection and credential span collection
// are frozen copies of the unoptimized procedures. Production versions carry
// ASCII and mandatory-byte shortcuts; the oracle must not inherit them.
func referenceNormalize(s string) string {
	return strings.Map(referenceNormalizeRune, s)
}

func referenceNormalizeRune(r rune) rune {
	switch {
	case unicode.Is(unicode.Cf, r), r == '\u034f', r >= '\ufe00' && r <= '\ufe0f', r >= '\U000e0100' && r <= '\U000e01ef':
		return -1
	case unicode.IsSpace(r) && r != ' ' && r != '\t' && r != '\r' && r != '\n':
		return ' '
	default:
		return r
	}
}

func referencePristineFloorRune(r rune) rune {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad':
		return -1
	}
	return r
}

func referenceProjectedSpans(source string, normalize func(rune) rune, collect func(string) []valueSpan) []valueSpan {
	view := strings.Map(normalize, source)
	if view == source {
		return collect(source)
	}
	// Each output byte points to its original rune. The terminal offset
	// includes removed controls at a match's trailing boundary safely.
	offsets := make([]int, 0, len(view)+1)
	for at, r := range source {
		if next := normalize(r); next >= 0 {
			for range len(string(next)) {
				offsets = append(offsets, at)
			}
		}
	}
	offsets = append(offsets, len(source))
	var spans []valueSpan
	for _, span := range collect(view) {
		spans = append(spans, valueSpan{offsets[span.start], offsets[span.end]})
	}
	return spans
}
