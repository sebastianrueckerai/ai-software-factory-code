package console

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Acceptance (roadmap 176, Stage 18 item 3.16): the whole flow of a filing
// with a GitHub import. The project is created and its import row waits as
// pending; the infra pull request closes without merging; the status panel
// read deletes the import row at once instead of leaving it to the purge,
// so a later merge registers an empty repository no more.
func TestImportRowDeletedWhenPRClosedUnmerged(t *testing.T) {
	s := newStatusRig(t)
	s.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }

	form := linkshortForm(s.csrf)
	form.Set("source", "acme/app")
	form.Set("token", ghToken)
	if rec := s.do(post("/projects", form), s.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects = %d: %s", rec.Code, rec.Body.String())
	}
	want := factoryd.Mirror{Project: "linkshort", Direction: factoryd.MirrorImport, Remote: "acme/app", Token: ghToken, State: "pending"}
	if got := s.mirrors.rows["linkshort import"]; got != want {
		t.Fatalf("import row after the filing %+v, want %+v", got, want)
	}
	if names, _, _ := steps(s.status(t)); !slices.Contains(names, "Imported from GitHub") {
		t.Errorf("panel steps %q, want the GitHub import step while the row waits pending", names)
	}

	s.infra.set(map[string]any{"number": 12, "state": "closed", "merged": false,
		"html_url": "https://git.example/sf/infra/pulls/12", "user": map[string]any{"login": "sebastian"}})
	body := s.status(t)
	_, states, _ := steps(body)
	if states[0] != stepFailed {
		t.Errorf("pull request step = %q, want failed", states[0])
	}
	if !slices.Equal(s.mirrors.deletes, []string{"linkshort import"}) {
		t.Errorf("DeleteMirror calls %q, want [linkshort import]", s.mirrors.deletes)
	}
	if _, ok := s.mirrors.rows["linkshort import"]; ok {
		t.Error("the closed unmerged pull request kept the import row")
	}
	if m, ok, err := s.mirrors.Mirror(context.Background(), "linkshort", factoryd.MirrorImport); err != nil || ok {
		t.Errorf("the import row reads (%v, %v), want gone", m, ok)
	}
}
