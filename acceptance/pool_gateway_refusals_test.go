package acceptance

// The refusal contract of the spend ceilings, and the one request that
// drives it: what the real gateway answers when a call's irreducible
// reservation no longer fits the free headroom of the day, the month or
// the project — the status, the decision header, the error body literal,
// the audit row — with no hold kept, nothing booked, no key minted and
// nothing proxied upstream. The request prices its prompt at one euro
// whatever its length, so a free headroom of EUR 0.50 fits no output
// token at all.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// gwChatFields is the request the proofs post: the task's session alias,
// a short prompt, no output limit asked — so the alias's own limit and
// the request's bytes alone make the hold's irreducible reservation.
var gwChatFields = map[string]json.RawMessage{
	"model":    json.RawMessage(`"small"`),
	"messages": json.RawMessage(`[{"role":"user","content":"the ceilings hold"}]`),
}

// gwChatBody is the fields as the gateway re-encodes them, so a proof
// knows the exact byte count the hold prices before it posts.
func gwChatBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(gwChatFields)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// gwBodyBytes is the request's bytes as the gateway counts them: the
// re-encoded body and the slack its output limit adds.
func gwBodyBytes(t *testing.T) int {
	t.Helper()
	return len(gwChatBody(t)) + 64
}

// gwPrice answers the alias's price for a request of bytes: an input
// rate that reserves exactly one euro for the prompt whatever its length
// — the reservation no spend ceiling may fall below without refusing —
// and an output rate of 4e-6 a token, up to 32768 tokens.
func gwPrice(gwBodyBytes int) policy.Price {
	return policy.Price{In: 1 / float64(gwBodyBytes), Out: 4e-6, MaxOutput: 32768}
}

// gwGateway stands a real gateway on the store, the proof's fakes and
// the stub upstream, the policy version the store holds in force: the
// routes a proof posts to, built fresh so every proof reads its version
// from the store, no cache waited out.
func gwGateway(store *accGatewayStore, minter *accGatewayMinter, prices accGatewayPrices, upstream string) http.Handler {
	return factorygateway.NewServer(factorygateway.ServerOptions{
		Auth: accGatewayAuth{task: gwCaller.ID}, Store: store, Minter: minter, Prices: prices, LiteLLMURL: upstream,
	}).Routes("pool-gateway-proof")
}

// gwRefused posts the request once and asserts the spend refusal's whole
// contract: 402 with the decision header and the error body literal
// word for word, the audit row of the decision — the reason, the task's
// unspent figure, the policy version — no hold kept, the task's spend
// unmoved, and nothing fell through to the backend: no key minted, no
// call proxied. It answers the hold request the handler fitted, the
// ceilings it carried.
func gwRefused(t *testing.T, what string, h http.Handler, body []byte, store *accGatewayStore,
	minter *accGatewayMinter, stub *accStubLiteLLM, version int64, decision, reason string) policy.HoldRequest {
	t.Helper()
	audits, holds := len(store.auditRows()), store.openHolds()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer the-sandbox-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("%s = %d:\n%s\nwant 402", what, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(factorygateway.HeaderDecision); got != decision {
		t.Errorf("%s's %s = %q, want %q", what, factorygateway.HeaderDecision, got, decision)
	}
	want, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": reason, "type": "budget_exceeded", "param": nil, "code": "402"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != string(want)+"\n" {
		t.Fatalf("%s's body =\n%s\nwant\n%s", what, rec.Body.String(), string(want)+"\n")
	}
	rows := store.auditRows()
	if len(rows) != audits+1 {
		t.Fatalf("%s wrote %d audit rows, want the one refusal", what, len(rows)-audits)
	}
	row := rows[len(rows)-1]
	spend, id := 0.0, version
	equal(t, what+"'s audit row",
		[]any{row.TaskID, row.Kind, row.Decision, row.Reason, row.ModelAlias, row.SpendEUR, row.PolicyVersion},
		[]any{gwCaller.ID, "", decision, reason, "small", &spend, &id})
	if got := store.openHolds(); got != holds {
		t.Errorf("%s kept %d holds, want the %d it started with", what, got, holds)
	}
	if got := store.spent(gwCaller.ID); got != 0 {
		t.Errorf("%s booked %f EUR on the task, want none", what, got)
	}
	if mints := minter.calls(); len(mints) != 0 {
		t.Errorf("%s minted %v, want no key", what, mints)
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Errorf("%s proxied %v, want no backend call", what, calls)
	}
	reqs := store.holdRequests()
	if len(reqs) != 1 {
		t.Fatalf("%s fitted %d hold requests, want the one", what, len(reqs))
	}
	return reqs[0]
}

// gwHoldRequest asserts the hold request the handler fitted carried the
// ceilings of the version in force — the day's and the month's from
// policy.yaml, the project's own cap from its file — and the prompt's
// irreducible one-euro reservation at the alias's priced limit.
func gwHoldRequest(t *testing.T, what string, r policy.HoldRequest, gwBodyBytes int) {
	t.Helper()
	equal(t, what+"'s hold request",
		[]any{r.TaskID, r.Kind, r.Ref, r.Session, r.Bytes, r.ImageTokens, r.WantOut,
			r.DayEUR, r.MonthEUR, r.Project, r.ProjectEUR, r.PremiumEUR},
		[]any{gwCaller.ID, policy.KindModel, "small", "", gwBodyBytes, 0, 0,
			gwDayEUR, gwMonthEUR, accProjectName, gwProjectCapEUR, 0.0})
	if r.Price.MaxOutput != 32768 {
		t.Errorf("%s's hold request carries the output limit %d, want the policy's 32768", what, r.Price.MaxOutput)
	}
	// the reservation itself is never printed — only the free headroom
	// and its figures are — so its float wobble cannot reach a literal
	if in := float64(r.Bytes) * r.Price.In; in < 0.999999 || in > 1.000001 {
		t.Errorf("%s's hold prices the prompt at %f EUR, want the irreducible one", what, in)
	}
}
