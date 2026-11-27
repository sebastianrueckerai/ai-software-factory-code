package gateway

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// maxGatewayDescription is the most words a gateway tool's description
// holds: what the tool does, what it takes and what it costs.
const maxGatewayDescription = 45

// Every tool the gateway can list, for a caller whose class allows each one
// and whose project runs in a namespace, says what it does in at most 45
// words.
func TestGatewayDescriptionsAreCapped(t *testing.T) {
	caps := map[string]policy.ToolCap{}
	for _, tool := range slices.Concat(policy.PaidTools, policy.FreeTools) {
		caps[tool] = policy.ToolCap{}
	}
	pv := &policy.Version{Tree: policy.Tree{
		Document: policy.Document{Classes: map[string]policy.Class{"all": {Tools: caps}}},
		Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Namespaces: []string{"code"}}},
	}}
	c := &toolCaller{pv: pv, task: Task{ID: "t-1", Classes: []string{"all"}, Repository: "sf/code"}}
	server, client := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewServer(ServerOptions{Cloud: &Cloud{}}).toolServer(c).Connect(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(policy.PaidTools) + len(policy.FreeTools); len(res.Tools) != want {
		t.Fatalf("the gateway lists %d tools, want every one of the %d", len(res.Tools), want)
	}
	for _, tl := range res.Tools {
		if n := len(strings.Fields(tl.Description)); n > maxGatewayDescription {
			t.Errorf("%s: its description holds %d words, want at most %d", tl.Name, n, maxGatewayDescription)
		}
	}
}
