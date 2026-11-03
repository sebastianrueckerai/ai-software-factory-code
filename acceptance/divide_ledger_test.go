package acceptance

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestDivideConquerLedgerIntegrity holds the ledger's integrity contract
// under the divide proofs' own producers: the settlement the spend
// writer books is one row per card however often it runs again, and the
// economics panels the proofs read stand on exactly the euros it booked
// — no double booking, no lost settlement. (The Runner-level spike —
// AutoMergeNote, ModelReview and SessionAnswered through
// interface-embedding fakes — stays open for the next session: this
// session's workspace mount flapped its go builds.)
func TestDivideConquerLedgerIntegrity(t *testing.T) {
	s, db := divideStore(t)
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	accEconCard(t, s, "t-ledger-1", "small", 100, 3, at)
	accEconCard(t, s, "t-ledger-2", "small", 300, 5, at)

	// the retried settlement books nothing again: one row per card.
	if again, err := s.SettleSpendFinal(context.Background(), "t-ledger-1", 3); err != nil || again {
		t.Errorf("retried settlement = (%v, %v), want (false, nil)", again, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM ledger`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows < 2 {
		t.Errorf("ledger holds %d rows, want the two settled cards", rows)
	}

	// the economics panel reads exactly the euros the settlement booked:
	// 8 EUR over 400 merged lines answers €0.0200.
	rig, _ := NewMetricsRig(t, s)
	holds(t, rig.page(), []string{
		"€0.0200",
		"2 cards",
	}, nil)
}
