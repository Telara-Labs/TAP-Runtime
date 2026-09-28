package validate

import (
	"fmt"
	"os"
	"strings"

	"telara.dev/tap/internal/diag"
	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/schemautil"
)

var (
	topLevelKeys        = []string{"apiVersion", "kind", "metadata", "interface", "requirements", "effects", "execution", "suggested_mode"}
	metadataKeys        = []string{"name", "publisher", "version", "description", "output_description", "license", "source", "artifactDigest", "forkOf"}
	interfaceKeys       = []string{"inputSchema", "outputSchema"}
	requirementsKeys    = []string{"network", "credentials", "browser", "reasoning"}
	networkKeys         = []string{"egressHosts"}
	egressHostEntryKeys = []string{"slot"}
	credentialKeys      = []string{"slot", "integration", "type", "actions", "accepts", "identity"}
	browserReqKeys      = []string{"session", "origins", "navigation", "downloads", "uploads", "clipboard"}
	originKeys          = []string{"slot", "origin"}
	reasoningKeys       = []string{"provider", "capabilities", "maxTokens", "dataClasses"}
	// effectsKeys deliberately omits "destructive": that field was removed
	// in v1 (CHANGELOG.md ruling 1) and gets its own named rejection below
	// rather than a generic unknown-field finding.
	effectsKeys = []string{"class", "idempotent"}
	// effectsClassValues is the full v1 5-value enum (CHANGELOG.md ruling 1).
	effectsClassValues = map[string]bool{"read": true, "write": true, "destructive": true, "financial": true, "identity-admin": true}
	executionKeys      = []string{"runtime", "entrypoint", "timeoutSeconds", "resumable", "may_suspend", "requires_features", "triggers"}
	// executionTriggerValues is CHANGELOG.md ruling 5's execution.triggers vocabulary.
	executionTriggerValues = map[string]bool{"manual": true, "schedule": true, "event": true}

	writeVerbPrefixes = []string{
		"create_", "update_", "delete_", "cancel_", "retry_", "trigger_", "merge_", "close_",
		"transition_", "post_", "send_", "upload_", "invite_", "assign_", "move_", "copy_",
		"archive_", "append_", "remove_", "protect_", "unprotect_", "start_", "add_", "set_",
		"accept_", "decline_", "fork_", "watch_", "unwatch_", "join_", "reindex_",
	}

	bannedNodeTypes = []string{"document_parse", "ocr"}
)

// ValidateManifest runs the primitive.yaml checks: closed field allowlist,
// JSON-Schema validity, description rules (03 §3.1), and credential-slot
// rules (03 §3.0). It does not cross-check against the workflow -- that
// happens in ValidateWorkflow / ValidateCrossPackage.
func ValidateManifest(pkg *model.Package) diag.Findings {
	var out diag.Findings
	m := pkg.Manifest

	out = append(out, checkClosed(m.Raw, topLevelKeys, "primitive.yaml")...)

	if m.APIVersion == "" {
		out = append(out, diag.Error(diag.ClassSchema, "missing-apiVersion", "primitive.yaml", "apiVersion is required", ""))
	} else if !strings.HasPrefix(m.APIVersion, "primitives.telara.dev/") {
		out = append(out, diag.Error(diag.ClassSchema, "bad-apiVersion", "primitive.yaml#apiVersion",
			fmt.Sprintf("unrecognized apiVersion %q", m.APIVersion), "use primitives.telara.dev/v1 or v1alpha1"))
	}
	if m.Kind != "Primitive" {
		out = append(out, diag.Error(diag.ClassSchema, "bad-kind", "primitive.yaml#kind",
			fmt.Sprintf("kind must be Primitive, got %q", m.Kind), ""))
	}

	out = append(out, checkClosed(m.Metadata.Raw, metadataKeys, "primitive.yaml#metadata")...)
	if m.Metadata.Name == "" {
		out = append(out, diag.Error(diag.ClassSchema, "missing-name", "primitive.yaml#metadata.name", "metadata.name is required", ""))
	}
	if m.Metadata.Version == "" {
		out = append(out, diag.Error(diag.ClassSchema, "missing-version", "primitive.yaml#metadata.version", "metadata.version is required", ""))
	}
	if m.Metadata.Description == "" {
		out = append(out, diag.Error(diag.ClassSchema, "missing-description", "primitive.yaml#metadata.description", "metadata.description is required", ""))
	} else {
		out = append(out, CheckDescription("primitive.yaml#metadata.description", m.Metadata.Description)...)
	}
	if m.Metadata.OutputDescription != "" {
		out = append(out, CheckDescription("primitive.yaml#metadata.output_description", m.Metadata.OutputDescription)...)
	}

	out = append(out, checkClosed(m.Interface.Raw, interfaceKeys, "primitive.yaml#interface")...)
	out = append(out, validateSchemaRef(pkg, "inputSchema", m.Interface.InputSchemaRef, m.Interface.InputSchemaInline)...)
	out = append(out, validateSchemaRef(pkg, "outputSchema", m.Interface.OutputSchemaRef, m.Interface.OutputSchemaInline)...)
	out = append(out, checkNowNotRequiredInInputSchema(pkg)...)

	out = append(out, checkClosed(m.Requirements.Raw, requirementsKeys, "primitive.yaml#requirements")...)
	out = append(out, checkClosed(m.Requirements.Network.Raw, networkKeys, "primitive.yaml#requirements.network")...)
	for i, h := range m.Requirements.Network.EgressHosts {
		if hm, ok := h.Raw.(map[string]interface{}); ok {
			out = append(out, checkClosed(hm, egressHostEntryKeys, fmt.Sprintf("primitive.yaml#requirements.network.egressHosts[%d]", i))...)
			if h.Slot == "" {
				out = append(out, diag.Error(diag.ClassSchema, "egress-slot-missing-name", fmt.Sprintf("primitive.yaml#requirements.network.egressHosts[%d]", i),
					"egress-host slot entry requires a slot name", ""))
			}
		}
	}
	for i, c := range m.Requirements.Credentials {
		path := fmt.Sprintf("primitive.yaml#requirements.credentials[%d]", i)
		out = append(out, checkClosed(c.Raw, credentialKeys, path)...)
		out = append(out, validateCredentialSlot(path, c)...)
	}
	if m.Requirements.Browser != nil {
		out = append(out, checkClosed(m.Requirements.Browser.Raw, browserReqKeys, "primitive.yaml#requirements.browser")...)
		for i, o := range m.Requirements.Browser.Origins {
			if om, ok := o.Raw.(map[string]interface{}); ok {
				out = append(out, checkClosed(om, originKeys, fmt.Sprintf("primitive.yaml#requirements.browser.origins[%d]", i))...)
			}
		}
	}
	if m.Requirements.Reasoning != nil {
		out = append(out, checkClosed(m.Requirements.Reasoning.Raw, reasoningKeys, "primitive.yaml#requirements.reasoning")...)
	}

	effectsForClosedCheck := m.Effects.Raw
	if _, present := m.Effects.Raw["destructive"]; present {
		out = append(out, diag.Error(diag.ClassSchema, "effects-destructive-removed", "primitive.yaml#effects.destructive",
			"effects.destructive was removed in v1; class carries it",
			"remove effects.destructive -- use effects.class (read|write|destructive|financial|identity-admin) to express severity"))
		// Filter "destructive" out before the generic closed-key check so it
		// doesn't ALSO surface as a redundant unknown-field finding: the
		// named rejection above is the actionable one.
		filtered := make(map[string]interface{}, len(m.Effects.Raw))
		for k, v := range m.Effects.Raw {
			if k != "destructive" {
				filtered[k] = v
			}
		}
		effectsForClosedCheck = filtered
	}
	out = append(out, checkClosed(effectsForClosedCheck, effectsKeys, "primitive.yaml#effects")...)
	if !effectsClassValues[m.Effects.Class] {
		out = append(out, diag.Error(diag.ClassSchema, "bad-effects-class", "primitive.yaml#effects.class",
			fmt.Sprintf("effects.class must be one of read|write|destructive|financial|identity-admin, got %q", m.Effects.Class), ""))
	}

	out = append(out, checkClosed(m.Execution.Raw, executionKeys, "primitive.yaml#execution")...)
	for _, tr := range m.Execution.Triggers {
		if !executionTriggerValues[tr] {
			out = append(out, diag.Error(diag.ClassSchema, "bad-execution-trigger", "primitive.yaml#execution.triggers",
				fmt.Sprintf("execution.triggers entry must be one of manual|schedule|event, got %q", tr), ""))
		}
	}
	if m.Execution.Entrypoint == "" {
		out = append(out, diag.Error(diag.ClassSchema, "missing-entrypoint", "primitive.yaml#execution.entrypoint", "execution.entrypoint is required", ""))
	} else if _, err := os.Stat(pkg.ResolvePath(m.Execution.Entrypoint)); err != nil {
		out = append(out, diag.Error(diag.ClassSchema, "entrypoint-not-found", "primitive.yaml#execution.entrypoint",
			fmt.Sprintf("entrypoint %q does not exist in package", m.Execution.Entrypoint), ""))
	}
	if m.Execution.TimeoutSeconds <= 0 {
		out = append(out, diag.Error(diag.ClassSchema, "bad-timeout", "primitive.yaml#execution.timeoutSeconds", "timeoutSeconds must be a positive integer", ""))
	}

	return out
}

func validateSchemaRef(pkg *model.Package, field, ref string, inline map[string]interface{}) diag.Findings {
	var out diag.Findings
	path := "primitive.yaml#interface." + field
	if ref == "" && inline == nil {
		out = append(out, diag.Error(diag.ClassSchema, "missing-schema", path, field+" must be a $ref or an inline JSON Schema object", ""))
		return out
	}
	resolvedRef := ""
	if ref != "" {
		resolvedRef = pkg.ResolvePath(ref)
		if _, err := os.Stat(resolvedRef); err != nil {
			out = append(out, diag.Error(diag.ClassSchema, "schema-ref-not-found", path,
				fmt.Sprintf("%s $ref %q does not exist", field, ref), ""))
			return out
		}
	}
	raw, _, err := schemautil.LoadSchemaDoc(resolvedRef, inline)
	if err != nil {
		out = append(out, diag.Error(diag.ClassSchema, "invalid-json-schema", path, err.Error(), "fix the schema so it compiles under JSON Schema 2020-12"))
		return out
	}
	schemautil.WalkDescriptions(raw, path, func(p, d string) {
		out = append(out, CheckDescription(p, d)...)
	})
	return out
}

// validateCredentialSlot enforces 03 §3.0: slots declare capability
// (actions), never auth mechanism, with two narrow exceptions
// (type: browser_session, and identity:). Findings are filed under
// diag.ClassContent (CHANGELOG.md v1 CLI fix item 6, "description/slot
// findings"): these rules are about what a slot's declaration commits to,
// not structural schema validity.
func validateCredentialSlot(path string, c model.CredentialSlot) diag.Findings {
	var out diag.Findings
	if c.Slot == "" {
		out = append(out, diag.Error(diag.ClassContent, "credential-missing-slot", path, "credential requires a slot name", ""))
	}
	isBrowserSession := c.Type == "browser_session"
	if !isBrowserSession && len(c.Actions) == 0 {
		out = append(out, diag.Error(diag.ClassContent, "credential-missing-actions", path,
			"credential slot must declare actions (capability), not just a type -- 03 §3.0: slots declare what the credential must DO",
			"add actions: [...] naming the class/operation/resource the slot needs"))
	}
	if c.Type != "" && !isBrowserSession {
		out = append(out, diag.Warn(diag.ClassContent, "credential-type-as-mechanism", path,
			fmt.Sprintf("type: %q may be pinning an auth mechanism rather than a capability class; only 'browser_session' is a legitimate mechanism-as-requirement exception", c.Type),
			"prefer expressing the requirement via actions; drop type unless it is genuinely a distinct capability class"))
	}
	if len(c.Accepts) > 0 {
		out = append(out, diag.Warn(diag.ClassContent, "credential-accepts-unjustified", path,
			"accepts: pins specific auth method(s); unjustified auth-method pinning is the canonical authoring overfit and silently excludes valid tenants (03 §3.0)",
			"remove accepts: unless this API surface genuinely behaves differently per auth method"))
	}
	return out
}

// checkNowNotRequiredInInputSchema enforces CHANGELOG.md v1 ruling 3: `now`
// is a reserved workflow input, auto-stamped by the runtime execution
// context (ISO-8601 datetime). It must never be declared required from the
// caller in the manifest's inputSchema -- see the workflow.yaml-side
// counterpart in workflow.go, which rejects `required: true` on the
// workflow input itself.
func checkNowNotRequiredInInputSchema(pkg *model.Package) diag.Findings {
	var out diag.Findings
	_, schema, err := loadInputSchemaRaw(pkg)
	if err != nil || schema == nil {
		return out
	}
	reqList, ok := model.AsSlice(schema["required"])
	if !ok {
		return out
	}
	for _, rv := range reqList {
		if s, ok := rv.(string); ok && s == "now" {
			out = append(out, diag.Error(diag.ClassSchema, "reserved-input-now-required", "primitive.yaml#interface.inputSchema.required",
				`"now" must not be listed in inputSchema.required -- it is a reserved auto-injected input (ISO-8601 datetime stamped by the runtime), never required from the caller`,
				`remove "now" from inputSchema.required; reference it as {from: inputs.now} without declaring it`))
		}
	}
	return out
}

// checkClosed rejects unknown keys per the CLOSED field allowlist rule.
func checkClosed(m map[string]interface{}, allowed []string, path string) diag.Findings {
	var out diag.Findings
	for _, k := range model.UnknownKeys(m, allowed) {
		out = append(out, diag.Error(diag.ClassSchema, "unknown-field", path+"."+k,
			fmt.Sprintf("unknown field %q (allowed: %s)", k, strings.Join(allowed, ", ")),
			"remove the field or check for a typo against the spec's closed allowlist"))
	}
	return out
}

func isWriteVerbTool(tool string) bool {
	for _, p := range writeVerbPrefixes {
		if strings.HasPrefix(tool, p) {
			return true
		}
	}
	return false
}

func isBannedNodeType(tool string) bool {
	for _, b := range bannedNodeTypes {
		if tool == b || strings.Contains(tool, b) {
			return true
		}
	}
	return false
}
