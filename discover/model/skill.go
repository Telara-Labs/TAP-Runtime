package model

// SkillProcedure is a pattern that sessions loading one skill run far more
// often than sessions that do not: a candidate for what that skill's
// recurring work actually is, step by step.
type SkillProcedure struct {
	Candidate
	// InSkill is how many of the skill's sessions contain the pattern, and
	// Coverage that as a share of them.
	InSkill  int     `json:"in_skill"`
	Coverage float64 `json:"coverage"`
	// Outside is how many other sessions contain it.
	Outside int `json:"outside"`
	// Lift is the pattern's rate in the skill's sessions over its rate
	// elsewhere. It is 0 when OnlyInSkill: the pattern never occurs outside
	// the skill, so the ratio has no finite value (and JSON has no infinity).
	Lift        float64 `json:"lift"`
	OnlyInSkill bool    `json:"only_in_skill"`
	// EnrichmentQ is the Fisher exact test's q-value, FDR-controlled over
	// every skill and pattern pair tested.
	EnrichmentQ float64 `json:"enrichment_q"`
}

// SkillReport lists, for one skill, the procedures its sessions share.
type SkillReport struct {
	Skill    string `json:"skill"`
	Sessions int    `json:"sessions"`
	// Significant is how many patterns were enriched in this skill's
	// sessions; Procedures holds the best of them that fix something.
	Significant int              `json:"significant"`
	Procedures  []SkillProcedure `json:"procedures"`
}
