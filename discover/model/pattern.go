package model

import (
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

// Pattern is an ordered list of step labels (as ids) and the sessions that
// contain it, each step within window steps of the one before.
type Pattern struct {
	Items    []int `json:"-"`
	Sessions []int `json:"-"` // indexes into the corpus, ascending
}

func (p Pattern) Key() string {
	var b strings.Builder
	for i, x := range p.Items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(util.Itoa(x))
	}
	return b.String()
}
