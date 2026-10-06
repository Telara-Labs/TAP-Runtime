// Package termart draws the TAP logo, progress bars and live status rows
// that the CLI shows on a terminal. Callers use it only when the stream is
// a terminal; a pipe or a test keeps the plain text.
package termart

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// AccentSGR is the CLI's accent color: xterm 99, the 256-color match for
// Telara's brand violet (#8B5BFF). ShineSGR is the lavender of the logo's
// sweep and the setup pulse.
const (
	AccentSGR = "38;5;99"
	ShineSGR  = "1;38;5;183"
	accent    = AccentSGR
)

var logo = []string{
	"████████╗ █████╗ ██████╗ ",
	"╚══██╔══╝██╔══██╗██╔══██╗",
	"   ██║   ███████║██████╔╝",
	"   ██║   ██╔══██║██╔═══╝ ",
	"   ██║   ██║  ██║██║     ",
	"   ╚═╝   ╚═╝  ╚═╝╚═╝     ",
}

// logoWidth is the columns the logo needs, version included.
const logoWidth = 40

// Spinner is the frame sequence of a running step.
var Spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Terminal reports whether w is a terminal.
func Terminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Wide reports whether w is a terminal wide enough for the TAP logo.
func Wide(w io.Writer) bool { return WideFor(w, logoWidth) }

// WideFor reports whether w is a terminal at least cols wide.
func WideFor(w io.Writer, cols int) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	got, _, err := term.GetSize(int(f.Fd()))
	return err == nil && got >= cols
}

// Paint wraps t in an SGR code when on.
func Paint(on bool, code, t string) string {
	if !on {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}

func Accent(on bool, t string) string { return Paint(on, accent, t) }
func Dim(on bool, t string) string    { return Paint(on, "2", t) }
func Good(on bool, t string) string   { return Paint(on, "32", t) }
func Bad(on bool, t string) string    { return Paint(on, "31", t) }

// Logo is the block-letter TAP logo with the version beside its last line.
func Logo(version string, color bool) string { return Art(logo, version, color) }

// Art is block art in the accent color, with the version beside its last
// line, framed by a blank line above and below.
func Art(art []string, version string, color bool) string {
	if version == "dev" {
		version = ""
	}
	var b strings.Builder
	b.WriteString("\n")
	for i, l := range art {
		b.WriteString(" " + Accent(color, l))
		if i == len(art)-1 && version != "" {
			b.WriteString(" " + Dim(color, "v"+strings.TrimPrefix(version, "v")))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// revealFrames is how many frames the logo takes to draw in, left to right.
const revealFrames = 12

// Final is the frame number Live passes for the block it leaves on screen.
const Final = -1

// Spin is the spinner glyph for a frame.
func Spin(frame int) string {
	if frame < 0 {
		frame = 0
	}
	return Spinner[frame%len(Spinner)]
}

// LogoFrame is one frame of the animated TAP logo; see ArtFrame.
func LogoFrame(version string, color bool, frame int) []string {
	return ArtFrame(logo, version, color, frame)
}

// ArtFrame is one frame of animated block art: for the first frames it draws
// in from the left, then a light band sweeps across it. The Final frame, and
// any frame without color, is the still art.
func ArtFrame(art []string, version string, color bool, frame int) []string {
	if version == "dev" {
		version = ""
	}
	if !color || frame == Final {
		return strings.Split(strings.TrimSuffix(strings.TrimPrefix(Art(art, version, color), "\n"), "\n"), "\n")
	}
	cols := len([]rune(art[0]))
	shown := cols
	if frame < revealFrames {
		shown = (frame + 1) * cols / revealFrames
	}
	// The band moves one column a frame and repeats with a pause after it
	// leaves the art; it leans so the sweep reads as a shine, not a cursor.
	period := cols + 24
	band := (frame - revealFrames) % period
	lines := make([]string, 0, len(art)+1)
	for i, l := range art {
		var b strings.Builder
		b.WriteString(" ")
		for j, r := range []rune(l) {
			switch {
			case j >= shown:
				b.WriteRune(' ')
			case r == ' ':
				b.WriteRune(r)
			case frame >= revealFrames && abs(j+i-band) <= 1:
				b.WriteString(Paint(true, ShineSGR, string(r)))
			default:
				b.WriteString(Paint(true, accent, string(r)))
			}
		}
		if i == len(art)-1 && version != "" && shown == cols {
			b.WriteString(" " + Dim(true, "v"+strings.TrimPrefix(version, "v")))
		}
		lines = append(lines, b.String())
	}
	return append(lines, "")
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Bar is a width-column progress bar for done of total.
func Bar(done, total, width int, color bool) string {
	if total <= 0 || width <= 0 {
		return ""
	}
	if done > total {
		done = total
	}
	full := done * width / total
	var b strings.Builder
	b.WriteString(strings.Repeat("━", full))
	rest := width - full
	if rest > 0 && done < total {
		b.WriteString("╸")
		rest--
	}
	return Accent(color, b.String()) + Dim(color, strings.Repeat("─", rest))
}

// Live redraws a block of lines in place while a step runs. Render gets the
// frame number, for spinners, and returns the block. Start begins redrawing;
// Stop draws the final block and leaves it on the screen.
type Live struct {
	out    io.Writer
	render func(frame int) []string
	every  time.Duration

	mu    sync.Mutex
	drawn int
	frame int
	stop  chan struct{}
	done  chan struct{}
}

func NewLive(out io.Writer, render func(frame int) []string) *Live {
	return &Live{out: out, render: render, every: 80 * time.Millisecond}
}

func (l *Live) Start() {
	l.stop, l.done = make(chan struct{}), make(chan struct{})
	l.Draw()
	go func() {
		defer close(l.done)
		t := time.NewTicker(l.every)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				l.Draw()
			}
		}
	}()
}

// Draw replaces the block drawn last with the current one.
func (l *Live) Draw() { l.draw(false) }

func (l *Live) draw(final bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	if l.drawn > 0 {
		b.WriteString("\r\x1b[" + itoa(l.drawn) + "A")
	}
	frame := l.frame
	if final {
		frame = Final
	}
	lines := l.render(frame)
	for _, line := range lines {
		b.WriteString("\r\x1b[K" + line + "\n")
	}
	// A shorter block than last time leaves no stale rows below it.
	for i := len(lines); i < l.drawn; i++ {
		b.WriteString("\r\x1b[K\n")
	}
	if extra := l.drawn - len(lines); extra > 0 {
		b.WriteString("\x1b[" + itoa(extra) + "A")
	}
	l.drawn = len(lines)
	l.frame++
	io.WriteString(l.out, b.String())
}

func (l *Live) Stop() {
	if l.stop != nil {
		close(l.stop)
		<-l.done
		l.stop = nil
	}
	l.draw(true)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
