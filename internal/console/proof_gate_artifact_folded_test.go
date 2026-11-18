package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// gate_artifact_folded_proven: the merge face prints its gate artifact
// only inside a closed details whose summary names the artifact and its
// size in KiB; a gate log past 8 KiB shows its first 8 KiB alone, and no
// artifact pre stands outside the fold.
func TestProofGateArtifactFolded(t *testing.T) {
	r := newRig(t)
	humanRig(r)
	ck, _ := r.loggedIn(t)
	long := strings.Repeat("0123456789", 900)
	for _, tc := range []struct{ content, summary, shown string }{
		{"sizes: 3 rows\ndoors: 2 rows", "pages_ok.txt at 8e3a1b0, 1 KiB", "sizes: 3 rows\ndoors: 2 rows"},
		{long, "pages_ok.txt at 8e3a1b0, the first 8 KiB of 9 KiB", long[:8192]},
	} {
		_ = r.c.cfg.Timelines.Put(t.Context(), factoryd.ArtifactKey("t-int", "pages_ok"), []byte(pagesOK(tc.content)))
		column := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
		fold := `<details class="fold"><summary>` + tc.summary + `</summary><pre class="artifact" tabindex="0">` + tc.shown + `</pre></details>`
		if n := strings.Count(column, fold); n != 1 {
			t.Errorf("an artifact of %d bytes: the closed fold %q stands %d times, want once:\n%s", len(tc.content), tc.summary, n, column)
		}
		if n := strings.Count(column, `<pre class="artifact"`); n != 1 {
			t.Errorf("an artifact of %d bytes: %d artifact pres on the face, want the fold's one", len(tc.content), n)
		}
	}
}
