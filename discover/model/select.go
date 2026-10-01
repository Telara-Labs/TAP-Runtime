package model

import (
	"time"
)

// Opportunity is the selection pass's judgment of one request.
type Opportunity struct {
	ID          string   `json:"id"`
	Client      string   `json:"client"`
	Session     string   `json:"session"`
	Request     int      `json:"request"`
	Recommended bool     `json:"recommended"`
	Route       string   `json:"route,omitempty"`
	Reasons     []string `json:"reasons"`
	// Task is the reference `tap discover brief --task` takes.
	Task string `json:"task"`
	// Contract is what the procedure is, as far as the evidence shows:
	// the grouping key. Start is when the request began.
	Contract string    `json:"contract,omitempty"`
	Start    time.Time `json:"start,omitempty"`
}

// Selection routes.
const (
	RouteStatedTemplate = "stated_template"
	RouteRerunCheck     = "rerun_check"
	RouteParamLoop      = "parametric_loop"
	RouteNamedObject    = "named_object"
)

// OpportunityGroup is every recommended request with the same contract.
type OpportunityGroup struct {
	Contract string    `json:"contract"`
	Route    string    `json:"route"`
	Requests int       `json:"requests"`
	Sessions int       `json:"sessions"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	// Example is the most recent member: the one to brief.
	Example Opportunity `json:"example"`
	Members []string    `json:"members"`
}
