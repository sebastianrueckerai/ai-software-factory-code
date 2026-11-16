package factoryd

import (
	"context"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// caseStore and caseIDs moved to the pg package with cases_pg_test.go;
// these copies stay for the tests still here that share them.

func caseStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(context.Background(), schematest.DSN(t, "cases"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(context.Background(), `TRUNCATE cases CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func caseIDs(cs []Case) []int64 {
	var out []int64
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}
