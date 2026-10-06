package discover

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Telara-Labs/TAP-Runtime/discover/termart"
)

// Version is the runner's version, shown beside the logo. The runner sets it.
var Version = ""

// readBoard is what tap discover shows on a terminal while it reads and
// analyses: the animated logo, one row per agent with its own session count,
// and the search with the step it is on. Every method is a no-op on a nil
// board, which is what pipes get.
type readBoard struct {
	live  *termart.Live
	color bool

	mu      sync.Mutex
	clients []string
	state   []int // 0 waiting, 1 reading, 2 read
	done    []int // files read so far, for readers that report it
	total   []int // files to read, 0 when the reader cannot tell
	count   []int // sessions found once read

	sessions  int // > 0 once searching
	stage     string
	stageDone int
	stageOf   int
	families  int
	finished  bool
}

func newReadBoard(out io.Writer, clients []string, color bool) *readBoard {
	n := len(clients)
	b := &readBoard{clients: clients, color: color, state: make([]int, n), done: make([]int, n), total: make([]int, n), count: make([]int, n)}
	b.live = termart.NewLive(out, b.render)
	b.live.Start()
	return b
}

func (b *readBoard) set(f func()) {
	if b == nil {
		return
	}
	b.mu.Lock()
	f()
	b.mu.Unlock()
}

func (b *readBoard) reading(i int) { b.set(func() { b.state[i] = 1 }) }

// progress is a reader's trace.Progress for agent i.
func (b *readBoard) progress(i int) func(done, total int) {
	return func(done, total int) { b.set(func() { b.done[i], b.total[i] = done, total }) }
}

func (b *readBoard) read(i, n int) { b.set(func() { b.state[i], b.count[i] = 2, n }) }

func (b *readBoard) analysing(n int) { b.set(func() { b.sessions, b.stage = n, "starting" }) }

// step is primitive.Stage for the search.
func (b *readBoard) step(name string, done, total int) {
	b.set(func() {
		b.stage = name
		if total > 0 {
			b.stageDone, b.stageOf = done, total
		}
	})
}

func (b *readBoard) analysed(n int) { b.set(func() { b.families, b.finished = n, true }) }

func (b *readBoard) stop() {
	if b == nil || b.live == nil {
		return
	}
	b.live.Stop()
	b.live = nil
}

const miniBar = 12

func (b *readBoard) render(frame int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.color
	spin := termart.Accent(c, termart.Spin(frame))
	lines := termart.LogoFrame(Version, c, frame)

	agents, sessions, name := 0, 0, 0
	for i, s := range b.state {
		switch s {
		case 2:
			agents++
			sessions += b.count[i]
		case 1:
			sessions += b.done[i]
		}
		if len(b.clients[i]) > name {
			name = len(b.clients[i])
		}
	}
	lines = append(lines, " "+termart.Accent(c, "▸")+" Reading agent history  "+termart.Bar(agents, len(b.clients), 24, c)+
		termart.Dim(c, fmt.Sprintf("  %d/%d agents · ", agents, len(b.clients)))+fmt.Sprint(sessions)+termart.Dim(c, " sessions"))
	for i, client := range b.clients {
		row := "   " + client + strings.Repeat(" ", name-len(client)) + "  "
		switch b.state[i] {
		case 0:
			row += termart.Dim(c, "· waiting")
		case 1:
			if b.total[i] > 0 {
				w := len(fmt.Sprint(b.total[i]))
				row += spin + " " + termart.Bar(b.done[i], b.total[i], miniBar, c) + fmt.Sprintf("  %*d", w, b.done[i]) + termart.Dim(c, fmt.Sprintf("/%d sessions", b.total[i]))
			} else {
				row += spin + " reading…"
			}
		default:
			row += termart.Good(c, "✓") + " " + termart.Bar(1, 1, miniBar, c) + fmt.Sprintf("  %d sessions", b.count[i])
		}
		lines = append(lines, row)
	}
	switch {
	case b.finished:
		lines = append(lines, " "+termart.Good(c, "✓")+fmt.Sprintf(" Found %d repeated tasks in %d sessions", b.families, b.sessions))
	case b.sessions > 0:
		row := " " + spin + " Looking for repeated work  "
		if b.stageOf > 0 {
			w := len(fmt.Sprint(b.stageOf))
			row += termart.Bar(b.stageDone, b.stageOf, 24, c) + fmt.Sprintf("  %*d", w, b.stageDone) + termart.Dim(c, fmt.Sprintf("/%d · %s", b.stageOf, b.stage))
		} else {
			row += termart.Dim(c, b.stage+"…")
		}
		lines = append(lines, row)
	}
	return lines
}
