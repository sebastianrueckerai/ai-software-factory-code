package console

import (
	"os"
	"strings"
	"testing"
)

// The chat input's drag bar painted its hover hint — "Drag to size the
// input; double-click resets it" — over the chevron ::after that is the
// bar's own affordance, and the 10px bar clipped it into unreadable
// overlap. The hint is gone; the bar keeps the hover highlight, the
// drag, the double-click reset and the shared data-tip painting every
// other quiet control uses.
func TestChatDragBarHoldsNoHoverHint(t *testing.T) {
	js, err := os.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	code := string(js)
	if strings.Contains(code, "Drag to size the input") {
		t.Errorf("FAULT STILL PRESENT: the drag bar's hover hint is still in chat.js")
	}
	for _, gone := range []string{"growbar\");\n\tbar.setAttribute(\"data-tip", "bar.setAttribute(\"data-tip\""} {
		if strings.Contains(code, gone) {
			t.Errorf("FAULT STILL PRESENT: the drag bar sets a data-tip: %q", gone)
		}
	}
	for _, kept := range []string{
		`bar.className = "growbar"`,
		`bar.setAttribute("role", "separator")`,
		`bar.setAttribute("aria-orientation", "horizontal")`,
		`bar.setAttribute("tabindex", "0")`,
		`bar.addEventListener("pointerdown"`,
		`bar.addEventListener("pointermove"`,
		`bar.addEventListener("dblclick"`,
	} {
		if !strings.Contains(code, kept) {
			t.Errorf("FIX MISSING: the drag bar lost %q", kept)
		}
	}
	css, err := os.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{
		`.growbar:hover::after, .growbar:focus-visible::after { opacity: 1; }`,
		`form.composer:has(> .growbar:hover)`,
		`[data-tip]:hover::after, [data-tip]:focus-visible::after { content: attr(data-tip);`,
	} {
		if !strings.Contains(string(css), kept) {
			t.Errorf("FIX MISSING: the stylesheet lost %q", kept)
		}
	}
}
