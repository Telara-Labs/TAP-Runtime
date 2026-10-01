package discover

import (
	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pipeline"
	"gitlab.com/telara-labs/tap-runtime/discover/routine"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// The package was split into layered subpackages (TENG-3084). These aliases
// keep the names telara-cli imports; new code should import the subpackage
// that owns the name. Methods that used to hang off Draft, Report and Routine
// are now functions (SaveDraft, PackageDraft, ReportDraft, RoutineDraft, ...).

type (
	Call          = trace.Call
	Session       = trace.Session
	Reader        = trace.Reader
	Draft         = model.Draft
	Report        = model.Report
	ReviewConfig  = routine.ReviewConfig
	ReviewActions = routine.ReviewActions
)

var (
	Run            = pipeline.Run
	DefaultOptions = pipeline.DefaultOptions
	DefaultReaders = history.DefaultReaders
	Review         = routine.Review
	WriteFunnel    = routine.WriteFunnel
	WriteText      = routine.WriteText
)
