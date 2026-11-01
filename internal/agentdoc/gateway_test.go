package agentdoc

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
)

// The fake gateway stands where the real one stands: it records the literal
// POST body and the purpose header of every chat call, and answers well
// enough that the real client completes.
func TestGatewayRecordsTheLiteralBody(t *testing.T) {
	g := NewGateway(`{"decision":"continue"}`)
	defer g.Close()
	body := `{"model":"big","max_tokens":1000,"response_format":{"type":"json_object"},"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`
	req, _ := http.NewRequest(http.MethodPost, g.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, "t-fixture")
	req.Header.Set(gateway.HeaderFactoryPurpose, "review")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	b, _ := io.ReadAll(res.Body)
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Choices) != 1 || out.Choices[0].Message.Content != `{"decision":"continue"}` {
		t.Fatalf("the answer carries the scripted content: %s", b)
	}
	recs := g.Bodies()
	if len(recs) != 1 {
		t.Fatalf("one recorded body, got %d", len(recs))
	}
	if string(recs[0].Raw) != body {
		t.Fatal("the recorded body is the literal POST body")
	}
	if recs[0].Purpose != "review" || recs[0].Task != "t-fixture" {
		t.Fatalf("the purpose and task headers are recorded: %+v", recs[0])
	}
}

// A streaming request gets a stream the real client can read to its [DONE].
func TestGatewayAnswersAStream(t *testing.T) {
	g := NewGateway("ok")
	defer g.Close()
	body := `{"model":"small","stream":true,"messages":[{"role":"user","content":"u"}]}`
	req, _ := http.NewRequest(http.MethodPost, g.URL+"/v1/chat/completions", strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	s := string(b)
	if !strings.Contains(s, `"delta"`) || !strings.Contains(s, "[DONE]") {
		t.Fatalf("the stream carries a delta and ends on [DONE]: %q", s)
	}
}
