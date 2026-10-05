package discover

import (
	"github.com/Telara-Labs/TAP-Runtime/discover/history"
	"github.com/Telara-Labs/TAP-Runtime/discover/model"
	"github.com/Telara-Labs/TAP-Runtime/discover/pipeline"
	"github.com/Telara-Labs/TAP-Runtime/discover/routine"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// The package was split into layered subpackages. These aliases
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
