package harness

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventJSONRoundTrip(t *testing.T) {
	in := Event{
		Time:   time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
		Kind:   EventToolCall,
		Detail: map[string]any{"tool": "bash", "exit_code": float64(0)},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Event
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Time.Equal(in.Time) || out.Kind != in.Kind {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	if out.Detail["tool"] != "bash" {
		t.Fatalf("detail = %v", out.Detail)
	}
}

func TestTaskBriefOmitsEmptyFields(t *testing.T) {
	data, err := json.Marshal(TaskBrief{Objective: "fix", Repository: "factory", SHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"issue_url", "findings", "artifacts"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("key %q present, want omitted", key)
		}
	}
}
