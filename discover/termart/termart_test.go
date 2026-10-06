package termart

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestLogoShowsVersionAndNoColorWhenOff(t *testing.T) {
	got := Logo("0.1.24", false)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("plain logo has escape codes:\n%s", got)
	}
	if !strings.Contains(got, "████████╗") || !strings.Contains(got, "v0.1.24") {
		t.Fatalf("logo lacks letters or version:\n%s", got)
	}
	if strings.Contains(Logo("dev", false), "vdev") {
		t.Fatal("a development build shows vdev")
	}
}

// Every animation frame has the same height and the same visible letters
// once drawn in, so redrawing in place never leaves stale rows.
func TestLogoFramesKeepTheirShape(t *testing.T) {
	still := LogoFrame("0.1.24", true, Final)
	for f := 0; f < 80; f++ {
		frame := LogoFrame("0.1.24", true, f)
		if len(frame) != len(still) {
			t.Fatalf("frame %d has %d lines, final has %d", f, len(frame), len(still))
		}
		if f >= revealFrames {
			for i := range frame {
				if sgr.ReplaceAllString(frame[i], "") != sgr.ReplaceAllString(still[i], "") {
					t.Fatalf("frame %d line %d differs in text from the final logo", f, i)
				}
			}
		}
	}
	first := sgr.ReplaceAllString(strings.Join(LogoFrame("", true, 0), ""), "")
	if strings.Count(first, "█") >= strings.Count(sgr.ReplaceAllString(strings.Join(still, ""), ""), "█") {
		t.Fatal("the first frame shows the whole logo; it should draw in")
	}
}

func TestBar(t *testing.T) {
	for _, c := range []struct {
		done, total int
		want        string
	}{
		{0, 4, "╸───────"},
		{2, 4, "━━━━╸───"},
		{4, 4, "━━━━━━━━"},
		{9, 4, "━━━━━━━━"},
	} {
		if got := Bar(c.done, c.total, 8, false); got != c.want {
			t.Errorf("Bar(%d,%d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
	if Bar(1, 0, 8, false) != "" {
		t.Error("a bar with no total should be empty")
	}
}

// Live moves the cursor back over the block it drew before drawing the next,
// and the block left on screen is the Final frame.
func TestLiveRedrawsInPlace(t *testing.T) {
	var out bytes.Buffer
	var frames []int
	l := NewLive(&out, func(f int) []string {
		frames = append(frames, f)
		return []string{"a", "b", "c"}
	})
	l.Draw()
	l.Draw()
	l.Stop()
	s := out.String()
	if strings.Count(s, "\x1b[3A") != 2 {
		t.Fatalf("want two moves up over 3 lines, got %q", s)
	}
	if frames[len(frames)-1] != Final {
		t.Fatalf("last frame drawn was %d, want Final", frames[len(frames)-1])
	}
	if Spin(Final) == "" {
		t.Fatal("Spin(Final) is empty")
	}
}
