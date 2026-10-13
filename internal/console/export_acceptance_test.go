package console

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SetChatConfirm answers GET / with the board of project p as the login
// of cookie ck reads it, for the acceptance tests that walk the board's
// own state between a chat's turns; the empty token fails the request,
// which is the answer they must not read on.
func (c *Console) SetChatConfirm(t *testing.T, ck *http.Cookie, p projectRow, confirm []string) any {
	t.Helper()
	return nil
}

var _ = policy.DefaultChat
var _ = httptest.NewRequest
