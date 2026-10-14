package factoryd

import (
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The scratchpad: empty before its first write; a write carries the hash
// of what it read, and a second write on the same base refuses and
// writes nothing (roadmap 239).
func TestPGStoreNotes(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_notes"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_notes`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	n, err := s.Notes(ctx, "sf/code")
	if err != nil || n.Content != "" {
		t.Fatalf("Notes before a write = %+v, %v, want empty", n, err)
	}
	empty := ContentHash("")
	if ok, err := s.PutNotes(ctx, "sf/code", "sebastian", "round 12: the cache plan", empty); err != nil || !ok {
		t.Fatalf("the first write = %v, %v", ok, err)
	}
	if ok, err := s.PutNotes(ctx, "sf/code", "anna", "a stale write", empty); err != nil || ok {
		t.Fatalf("a write on the stale base = %v, %v, want refused", ok, err)
	}
	n, _ = s.Notes(ctx, "sf/code")
	if n.Content != "round 12: the cache plan" || n.By != "sebastian" || n.Hash() != ContentHash("round 12: the cache plan") {
		t.Fatalf("Notes = %+v", n)
	}
	if other, _ := s.Notes(ctx, "sf/other"); other.Content != "" {
		t.Fatalf("another project's scratchpad = %+v, want empty", other)
	}
}
