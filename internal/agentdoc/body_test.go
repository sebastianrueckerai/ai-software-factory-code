package agentdoc

import (
	"encoding/json"
	"testing"
)

// ParseBody reads the envelope the renderer labels: which model alias, which
// knobs, which messages, which tools.
func TestParseBodyReadsTheSupervisorEnvelope(t *testing.T) {
	raw := []byte(`{"model":"big","max_tokens":1000,"response_format":{"type":"json_object"},"messages":[{"role":"system","content":"sys"},{"role":"user","content":"usr"}]}`)
	b, err := ParseBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Model != "big" || b.MaxTokens != 1000 || b.ResponseFormat != "json_object" || b.Stream {
		t.Fatalf("envelope fields, got %+v", b)
	}
	if len(b.Messages) != 2 || b.Messages[0].Role != "system" || b.Messages[0].Content != "sys" || b.Messages[1].Content != "usr" {
		t.Fatalf("messages, got %+v", b.Messages)
	}
}

func TestParseBodyReadsTheOperatorEnvelope(t *testing.T) {
	raw := []byte(`{"model":"small","stream":true,"max_tokens":200,"tools":[{"type":"function","function":{"name":"grep","description":"d"}},{"type":"function","function":{"name":"run","description":"r"}}],"messages":[{"role":"user","content":"u"}]}`)
	b, err := ParseBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Stream || b.ResponseFormat != "" || b.MaxTokens != 200 {
		t.Fatalf("envelope fields, got %+v", b)
	}
	if len(b.Tools) != 2 || b.Tools[0].Name != "grep" || b.Tools[1].Name != "run" {
		t.Fatalf("tool names, got %+v", b.Tools)
	}
	var probe struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b.Tools[1].Raw, &probe); err != nil || probe.Function.Name != "run" {
		t.Fatalf("each tool keeps its raw json, got %s", b.Tools[1].Raw)
	}
}

// The vision call's user content is a structured part list, not a string.
func TestParseBodyReadsStructuredContent(t *testing.T) {
	raw := []byte(`{"model":"vision","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`)
	b, err := ParseBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := b.Messages[0]
	if m.Content != "" || len(m.Parts) != 2 {
		t.Fatalf("structured content comes as parts, got %+v", m)
	}
	if m.Parts[0].Type != "text" || m.Parts[0].Text != "what is this" {
		t.Fatalf("text part, got %+v", m.Parts[0])
	}
	if m.Parts[1].Type != "image_url" || m.Parts[1].ImageURL != "data:image/png;base64,aGk=" {
		t.Fatalf("image part, got %+v", m.Parts[1])
	}
}
