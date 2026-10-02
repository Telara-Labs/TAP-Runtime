package primitive

import (
	"bytes"
	"strings"
	"testing"
)

func newTUI(res Result) *tui {
	shown, hidden := Triage(res, Ledger{})
	t := &tui{res: res, s: style{}, shown: shown, hidden: hidden, byID: map[string]Primitive{}, choice: make([]string, len(shown)), w: 120, h: 40}
	for _, p := range res.Primitives {
		t.byID[p.ID] = p
	}
	return t
}

func press(t *tui, input string) bool {
	for _, k := range splitKeys(input) {
		if t.key(k) {
			return true
		}
	}
	return false
}

func TestSplitKeys(t *testing.T) {
	got := strings.Join(splitKeys("jj\x1b[A\x1b[6~\r"), "|")
	if got != "j|j|\x1b[A|\x1b[6~|\r" {
		t.Fatalf("keys = %q", got)
	}
}

func TestSplitInputBuffersFragmentedMouseReport(t *testing.T) {
	keys, rest := splitInput("j\x1b[<64;12;")
	if got := strings.Join(keys, "|"); got != "j" {
		t.Fatalf("first read keys = %q", got)
	}
	if rest != "\x1b[<64;12;" {
		t.Fatalf("pending report = %q", rest)
	}
	keys, rest = splitInput(rest + "8M\x1b[<65;12;8M")
	if got := strings.Join(keys, "|"); got != "\x1b[<64;12;8M|\x1b[<65;12;8M" {
		t.Fatalf("completed reports = %q", got)
	}
	if rest != "" {
		t.Fatalf("unexpected pending bytes = %q", rest)
	}
}

func TestMouseWheelRoutesToDiscoverViews(t *testing.T) {
	ui := newTUI(twoFamilies())
	if delta, ok := mouseWheelDelta("\x1b[<64;12;8M"); !ok || delta != -1 {
		t.Fatalf("wheel up = %d, %v", delta, ok)
	}
	if delta, ok := mouseWheelDelta("\x1b[<65;12;8M"); !ok || delta != 1 {
		t.Fatalf("wheel down = %d, %v", delta, ok)
	}
	if _, ok := mouseWheelDelta("\x1b[<0;12;8M"); ok {
		t.Fatal("ordinary mouse click was treated as a wheel event")
	}
	ui.key("\x1b[<65;12;8M")
	if ui.cursor != 1 {
		t.Fatalf("list wheel did not move selection: cursor=%d", ui.cursor)
	}
	ui.view, ui.cursor, ui.scroll = cardView, 0, 0
	ui.key("\x1b[<65;12;8M")
	if ui.scroll != 1 || ui.cursor != 0 {
		t.Fatalf("card wheel changed scroll=%d cursor=%d", ui.scroll, ui.cursor)
	}
	ui.key("\x1b[<64;12;8M")
	if ui.scroll != 0 {
		t.Fatalf("wheel up did not scroll card back: %d", ui.scroll)
	}
}

func TestTUIListCardReviewSubmit(t *testing.T) {
	ui := newTUI(twoFamilies())
	if len(ui.shown) != 2 {
		t.Fatalf("fixture: %d", len(ui.shown))
	}
	// Move down, back up, open the first card, decline it (moves to the
	// second), accept the second (opens the review), submit.
	if press(ui, "\x1b[B\x1b[A\r") || ui.view != cardView || ui.cursor != 0 {
		t.Fatalf("open card: view %d cursor %d", ui.view, ui.cursor)
	}
	if press(ui, "d") || ui.cursor != 1 || ui.choice[0] != "deny" {
		t.Fatalf("decline: %v", ui.choice)
	}
	if press(ui, "a") || ui.view != reviewView || ui.choice[1] != "accept" {
		t.Fatalf("accept last: view %d %v", ui.view, ui.choice)
	}
	if !press(ui, "s") {
		t.Fatal("s on the review did not submit")
	}
}

func TestTUIQuitWritesNothingAndToggleClears(t *testing.T) {
	ui := newTUI(twoFamilies())
	press(ui, "a")
	if ui.choice[0] != "accept" {
		t.Fatal("a did not choose")
	}
	press(ui, "a")
	if ui.choice[0] != "" {
		t.Fatal("pressing a again did not clear the choice")
	}
	if press(ui, "q") || ui.view != doneView {
		t.Fatal("q did not quit without submitting")
	}
}

func TestTUIDrawFitsTheScreen(t *testing.T) {
	ui := newTUI(twoFamilies())
	var out bytes.Buffer
	for _, v := range []view{listView, cardView, reviewView} {
		ui.view = v
		var body bytes.Buffer
		switch v {
		case listView:
			ui.drawList(&body)
		case cardView:
			card(&body, ui.s, 1, len(ui.shown), ui.shown[0], ui.byID, "", ui.res.Summary)
		case reviewView:
			ui.drawReview(&body)
		}
		for _, l := range strings.Split(body.String(), "\n") {
			if width(fit(l, 80)) > 80 {
				t.Fatalf("line wider than the screen after fit: %q", l)
			}
		}
		out.Write(body.Bytes())
	}
	if !strings.Contains(out.String(), "Discover") || !strings.Contains(out.String(), "What it does") {
		t.Fatalf("views missing content:\n%s", out.String())
	}
}
