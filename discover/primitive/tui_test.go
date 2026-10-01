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
			card(&body, ui.s, 1, len(ui.shown), ui.shown[0], ui.byID, "")
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
	if !strings.Contains(out.String(), "TAP Discover") || !strings.Contains(out.String(), "Structure") {
		t.Fatalf("views missing content:\n%s", out.String())
	}
}
