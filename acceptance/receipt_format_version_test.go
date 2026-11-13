package acceptance

// Package acceptance holds the plan's end-to-end tests. This file is
// receipt_format_version (roadmap 66): the receipt document the
// foundation card raised carries format version 2 under its version-2
// shape — task, repository, issue and pr inside the facts object, the
// diff's hash named diff_sha256 — and a PutMergeReceipt to
// MergeReceipt round-trip keeps that version and shape byte for byte,
// so the compliance evidence export (roadmap 328) reads it by version.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestReceiptFormatVersion proves the raised format version on a
// receipt built from seeded records: the constant is 2, the marshalled
// document's version field says 2, the four task identifiers sit inside
// the facts object (never at the document's top level, where version 1
// carried the task), the diff's hash field is diff_sha256 (never the
// version-1 diff_hash), and the document stored through PutMergeReceipt
// and read back through MergeReceipt keeps its version and shape byte
// for byte.
func TestReceiptFormatVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := accReceiptStore(t, "acceptance_receipt_version")
	objects := &accObjects{files: map[string][]byte{}}
	r := accReceiptRunner(t, s, serve(t, &accReceiptForgejo{head: "c1-head", mergeCommit: "m9", diff: accReceiptDiff}), objects)
	const id = "rv1"
	accSeedReceipt(ctx, t, s, objects, id, 86)

	if factoryd.MergeReceiptVersion != 2 {
		t.Fatalf("MergeReceiptVersion = %d, want 2", factoryd.MergeReceiptVersion)
	}

	rec, err := r.BuildMergeReceipt(ctx, id, 9)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["version"] != float64(2) {
		t.Fatalf("the document's version = %v, want 2:\n%s", doc["version"], b)
	}

	// the version-2 shape: the identifiers inside the facts object, the
	// diff's hash named diff_sha256, neither anywhere else
	facts, ok := doc["facts"].(map[string]any)
	if !ok {
		t.Fatalf("the document's facts = %T, want the facts object:\n%s", doc["facts"], b)
	}
	sum := sha256.Sum256([]byte(accReceiptDiff))
	for key, want := range map[string]any{
		"task": id, "repository": "sf/code", "issue": float64(86), "pr": float64(9),
		"diff_sha256": hex.EncodeToString(sum[:]),
	} {
		if facts[key] != want {
			t.Errorf("facts.%s = %v, want %v", key, facts[key], want)
		}
		if _, top := doc[key]; top {
			t.Errorf("the document carries %s at its top level, want it inside facts alone", key)
		}
	}
	if _, old := facts["diff_hash"]; old {
		t.Errorf("the document still names the diff's hash diff_hash, want diff_sha256:\n%s", b)
	}
	if _, top := doc["diff_hash"]; top {
		t.Errorf("the document carries the version-1 diff_hash at its top level:\n%s", b)
	}

	// stored and read back: the version and the shape stand, byte for byte
	if _, err := s.PutMergeReceipt(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	back, found, err := s.MergeReceipt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("MergeReceipt found no stored receipt for %s", id)
	}
	if back.Version != factoryd.MergeReceiptVersion {
		t.Fatalf("the read-back receipt's version = %d, want %d (MergeReceiptVersion)", back.Version, factoryd.MergeReceiptVersion)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(b) {
		t.Fatalf("the read-back document =\n%s\nwant the stored one, byte for byte:\n%s", again, b)
	}
}
