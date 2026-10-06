package main

import (
	"bytes"
	"io"
	"strings"
	"sync"

	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/termart"
)

// setupBoard is what tap setup shows on a terminal: the animated logo over a
// diagram of TAP connecting to each agent. While an agent is being connected
// a pulse runs down its line; the line then shows how it ended, with the
// first thing installOne said about it. Every method is a no-op on a nil
// board, which is what pipes, --print and --remove get.
type setupBoard struct {
	live *termart.Live

	mu    sync.Mutex
	names []string
	state []int // 0 waiting, 1 connecting, 2 connected, 3 skipped, 4 failed
	said  []*bytes.Buffer
}

func newSetupBoard(out io.Writer, targets []agents.Client) *setupBoard {
	b := &setupBoard{state: make([]int, len(targets)), said: make([]*bytes.Buffer, len(targets))}
	for _, c := range targets {
		b.names = append(b.names, c.Name)
	}
	b.live = termart.NewLive(out, b.render)
	b.live.Start()
	return b
}

// begin returns where agent i's messages go: the board collects them to show
// on its line; without a board they go where they always did.
func (b *setupBoard) begin(i int, stdout, stderr io.Writer) (io.Writer, io.Writer) {
	if b == nil {
		return stdout, stderr
	}
	b.mu.Lock()
	b.state[i] = 1
	b.said[i] = &bytes.Buffer{}
	w := boardWriter{mu: &b.mu, buf: b.said[i]}
	b.mu.Unlock()
	return w, w
}

func (b *setupBoard) end(i, rc int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.state[i] = setupOutcome(rc, b.said[i].String())
	b.mu.Unlock()
	b.live.Draw()
}

// stop leaves the final diagram on screen, then prints in full what any
// failed agent said, so nothing a line had to shorten is lost.
func (b *setupBoard) stop(stdout, stderr io.Writer) {
	if b == nil {
		return
	}
	b.live.Stop()
	stopFailures(b, stdout, stderr)
}

func stopFailures(b *setupBoard, stdout, stderr io.Writer) {
	connected := false
	for i, s := range b.state {
		if s == 4 {
			io.WriteString(stderr, strings.TrimRight(b.said[i].String(), "\n")+"\n")
		}
		connected = connected || s == 2
	}
	if connected {
		io.WriteString(stdout, " Start a new session in each agent to use TAP.\n")
	}
}

// setupOutcome reads how connecting one agent ended from its exit code and
// what it said: installOne reports a missing program as skipped, not failed.
func setupOutcome(rc int, said string) int {
	switch {
	case rc != 0:
		return 4
	case strings.Contains(said, "skipped") || strings.Contains(said, "not on this machine"):
		return 3
	default:
		return 2
	}
}

// setupSummary is the short word for how connecting an agent ended; a failure
// keeps the first line it said, which is the error.
func setupSummary(state int, said string) string {
	switch {
	case state == 3:
		return "not installed"
	case state == 4:
		line, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
		if len(line) > 60 {
			line = line[:59] + "…"
		}
		return line
	case strings.Contains(said, "already"):
		return "already connected"
	default:
		return "connected"
	}
}

const wire = 14

func (b *setupBoard) render(frame int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := termart.LogoFrame(version, true, frame)
	lines = append(lines, " Connecting TAP to your agents", "")
	width := 0
	for _, n := range b.names {
		if len(n) > width {
			width = len(n)
		}
	}
	for i, n := range b.names {
		hub := "   "
		switch {
		case len(b.names) == 1:
			hub += termart.Accent(true, "TAP ━━")
		case i == 0:
			hub += termart.Accent(true, "TAP ━┳")
		case i == len(b.names)-1:
			hub += termart.Accent(true, "     ┗")
		default:
			hub += termart.Accent(true, "     ┣")
		}
		label := n + strings.Repeat(" ", width-len(n))
		var line, status string
		switch b.state[i] {
		case 0:
			line = termart.Dim(true, strings.Repeat("╌", wire)+"  "+label)
			status = termart.Dim(true, "· waiting")
		case 1:
			pos := max(frame, 0) % wire
			line = termart.Accent(true, strings.Repeat("━", pos)) + termart.Paint(true, termart.ShineSGR, "●") +
				termart.Dim(true, strings.Repeat("─", wire-pos-1)) + "  " + label
			status = termart.Accent(true, termart.Spin(frame)) + " connecting…"
		case 2:
			line = termart.Accent(true, strings.Repeat("━", wire-1)+"▶") + "  " + termart.Paint(true, "1", label)
			status = termart.Good(true, "✓") + " " + setupSummary(b.state[i], b.said[i].String())
		case 3:
			line = termart.Dim(true, strings.Repeat("─ ", wire/2)+"  "+label)
			status = termart.Dim(true, "· "+setupSummary(b.state[i], b.said[i].String()))
		default:
			line = termart.Bad(true, strings.Repeat("━", wire/2-1)+"╳"+strings.Repeat(" ", wire/2)) + "  " + label
			status = termart.Bad(true, "✗") + " " + setupSummary(b.state[i], b.said[i].String())
		}
		lines = append(lines, hub+line+"   "+status)
	}
	return append(lines, "")
}

// boardWriter appends to an agent's buffer under the board's lock, since the
// board reads it while drawing.
type boardWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w boardWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
