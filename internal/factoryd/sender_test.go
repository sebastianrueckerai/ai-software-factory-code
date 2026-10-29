package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The outbox's sender (roadmap 145) against fake venues and a real store:
// a due row sends once with the project's name and the card's link; a
// row whose ask settled before its send says nothing; a member who no
// longer writes the project loses their level and their row; the venue's
// rate limit defers without a try; a refused chat unlinks; a busy venue
// backs off through the whole ladder before it fails closed; a sent ask
// edits to its reason once the settle rule closes it, and skips the edit
// without a panic when its venue left the config.
func TestSender(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			chat, _ := payload["chat_id"].(string)
			switch chat {
			case "2":
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`)
			case "3", "3b", "3c":
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"ok":false,"description":"boom"}`)
			case "4":
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`)
			case "1", "5", "7":
				line := fmt.Sprintf("sendMessage %v %v", chat, payload["text"])
				if markup, ok := payload["reply_markup"].(map[string]any); ok {
					btn := markup["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
					line += fmt.Sprintf(" %v %v", btn["text"], btn["url"])
				}
				calls = append(calls, line)
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%s}}`, chat)
			default:
				t.Errorf("the unexpected telegram chat %q", chat)
			}
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			line := fmt.Sprintf("editMessageText %v %v %v", payload["chat_id"], payload["message_id"], payload["text"])
			if markup, ok := payload["reply_markup"].(map[string]any); ok {
				btn := markup["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
				line += fmt.Sprintf(" %v %v", btn["text"], btn["url"])
			}
			calls = append(calls, line)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("the unexpected telegram call %s", r.URL.Path)
		}
	}))
	defer api.Close()
	discordAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "POST" && r.URL.Path == "/channels/d-9/messages" {
			b, _ := io.ReadAll(r.Body)
			calls = append(calls, fmt.Sprintf("say %s", strings.TrimSpace(string(b))))
			fmt.Fprint(w, `{"id":"m-9"}`)
			return
		}
		t.Errorf("the unexpected discord call %s %s", r.Method, r.URL.Path)
	}))
	defer discordAPI.Close()

	dsn := schematest.DSN(t, "sender")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	for _, q := range []string{`TRUNCATE messenger_codes, messenger_links, notify_levels, notify_deliveries, chat_messages, tasks, events, projects CASCADE`,
		`DELETE FROM policy_activations`, `DELETE FROM policy_versions`} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, _, err := policy.Record(ctx, s.db, "sender-test", policy.Tree{Document: policy.Document{Approvers: []string{"anna"}},
		Projects: map[string]policy.ProjectConfig{
			"demo":  {Repository: "sf/code"},
			"other": {Repository: "sf/other"}}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo", "other"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO projects (name) VALUES ($1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, status, budget_eur, track) VALUES ('T-1', 'waiting-human', 0, 'direct')`); err != nil {
		t.Fatal(err)
	}

	bind := func(login, venue, address string) int64 {
		t.Helper()
		code, err := s.IssueMessengerCode(ctx, login)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.ConsumeMessengerCode(ctx, code, venue, address, "u-"+login); err != nil || !ok {
			t.Fatalf("bind %s: %v %v", login, ok, err)
		}
		links, err := s.MessengerLinks(ctx, login)
		if err != nil || len(links) != 1 {
			t.Fatalf("the links of %s = %+v, %v", login, links, err)
		}
		return links[0].ID
	}
	msg := func(repo, content, kind, task string, at time.Time) int64 {
		t.Helper()
		var id int64
		if err := s.db.QueryRowContext(ctx, `INSERT INTO chat_messages (repository, login, role, content, kind, task_id, at)
			VALUES ($1, 'factory', 'assistant', $2, $3, $4, $5) RETURNING id`, repo, content, kind, task, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	queue := func(message, link int64) {
		t.Helper()
		if err := s.QueueDelivery(ctx, message, link); err != nil {
			t.Fatal(err)
		}
	}
	// row reads the delivery of one link by its ref, "" for a row that
	// never sent; next reads a pending row's next try.
	row := func(link int64, ref string) (state string, tries int, lastError string) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT state, tries, last_error FROM notify_deliveries
			WHERE link_id = $1 AND ref = $2`, link, ref).Scan(&state, &tries, &lastError); err != nil {
			t.Fatalf("the row of link %d ref %q: %v", link, ref, err)
		}
		return
	}
	next := func(link int64) time.Time {
		t.Helper()
		var at time.Time
		if err := s.db.QueryRowContext(ctx, `SELECT next_try FROM notify_deliveries WHERE link_id = $1 AND state = 'pending'`, link).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	gone := func(message int64) bool {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notify_deliveries WHERE message_id = $1`, message).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	}
	called := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}

	when := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	bob, alice, carla, dana, eric, gina, hana, judy, karl, frank, ira :=
		bind("bob", VenueTelegram, "1"), bind("alice", VenueTelegram, "2"), bind("carla", VenueTelegram, "3"),
		bind("dana", VenueTelegram, "3b"), bind("eric", VenueTelegram, "4"), bind("gina", VenueDiscord, "d-9"),
		bind("hana", VenueTelegram, "5"), bind("judy", VenueTelegram, "3c"), bind("karl", VenueTelegram, "8"),
		bind("frank", VenueTelegram, "6"), bind("ira", VenueTelegram, "7")
	if err := s.SetNotifyLevel(ctx, "frank", "sf/other", "all"); err != nil {
		t.Fatal(err)
	}

	// a sent ask row, waiting for its settle edit
	askRow := msg("sf/code", "the card asks a question", MessageEscalation, "T-gone", when)
	queue(askRow, bob)
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET state = 'sent', ref = '77', sent_at = now()`); err != nil {
		t.Fatal(err)
	}

	// the first pass: one send per venue, the settled and the unentitled
	// rows say nothing, the failures take their paths
	headsUp, stumble, otherHeadsUp :=
		msg("sf/code", "the run finished", MessagePoke, "T-7", when),
		msg("sf/code", "the run stumbled", MessagePoke, "T-8", when),
		msg("sf/other", "the run finished", MessagePoke, "T-7", when)
	settledRow := msg("sf/code", "the card asks a question", MessageEscalation, "T-gone", when)
	openAsk := msg("sf/code", "the card asks a question", MessageEscalation, "T-1", when)
	queue(headsUp, bob)
	queue(settledRow, bob)
	queue(stumble, alice)
	queue(stumble, carla)
	queue(stumble, dana)
	queue(stumble, eric)
	queue(headsUp, gina)
	queue(openAsk, hana)
	queue(otherHeadsUp, frank)
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET tries = 5 WHERE message_id = $1 AND link_id = $2`, stumble, dana); err != nil {
		t.Fatal(err)
	}
	// judy's row enters on its fifth try: the last rung is still owed
	rung := msg("sf/code", "the run stumbled", MessagePoke, "T-8", when)
	queue(rung, judy)
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET tries = 4 WHERE message_id = $1 AND link_id = $2`, rung, judy); err != nil {
		t.Fatal(err)
	}

	sd := &Sender{Store: s, PublicURL: "https://console.example",
		Telegram: &Telegram{Token: "bot-key", BaseURL: api.URL},
		Discord:  &Discord{BotToken: "bot-token", BaseURL: discordAPI.URL},
		Lists: func(context.Context) (string, map[string][]string, error) {
			return "", map[string][]string{"sf/code": {"bob", "alice", "carla", "dana", "eric", "gina", "hana", "judy", "ira"}, "sf/other": {"bob"}}, nil
		}}
	if err := sd.Pass(ctx); err != nil {
		t.Fatal(err)
	}

	equalStrings(t, "the first pass", called(), []string{
		"editMessageText 1 77 demo: the card asks a question · moved on · 12:00 Open the card https://console.example/#card=T-gone",
		"sendMessage 1 demo: the run finished Open the card https://console.example/#card=T-7",
		`say {"components":[{"components":[{"label":"Open the card","style":5,"type":2,"url":"https://console.example/#card=T-7"}],"type":1}],"content":"demo: the run finished"}`,
		"sendMessage 5 demo: the card asks a question Open the card https://console.example/#card=T-1"})
	if state, _, _ := row(bob, "1"); state != "sent" {
		t.Fatalf("bob's row = %s", state)
	}
	if state, tries, _ := row(alice, ""); state != "pending" || tries != 0 || next(alice).Before(time.Now().Add(6*time.Second)) {
		t.Fatalf("the 429 row = %s %d %v", state, tries, next(alice))
	}
	if state, tries, why := row(carla, ""); state != "pending" || tries != 1 || next(carla).Before(time.Now().Add(29*time.Second)) || why == "" {
		t.Fatalf("the 500 row = %s %d %v %q", state, tries, next(carla), why)
	}
	if state, tries, why := row(dana, ""); state != "failed" || tries != 6 || why == "" {
		t.Fatalf("the failed row = %s %d %q", state, tries, why)
	}
	if state, tries, _ := row(judy, ""); state != "pending" || tries != 5 || next(judy).Before(time.Now().Add(39*time.Minute)) {
		t.Fatalf("the fifth try = %s %d %v", state, tries, next(judy))
	}
	if links, _ := s.MessengerLinks(ctx, "eric"); len(links) != 0 {
		t.Fatal("eric's link survived the refused chat")
	}
	var ericRows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notify_deliveries WHERE link_id = $1`, eric).Scan(&ericRows); err != nil {
		t.Fatal(err)
	}
	if !gone(settledRow) || ericRows != 0 {
		t.Fatal("the settled and the refused rows survived")
	}
	if !gone(otherHeadsUp) {
		t.Fatal("frank's row survived the lost access")
	}
	var level int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notify_levels WHERE login = 'frank' AND repository = 'sf/other'`).Scan(&level); err != nil || level != 0 {
		t.Fatalf("frank's level = %d, %v", level, err)
	}
	var editedAt interface{}
	if err := s.db.QueryRowContext(ctx, `SELECT edited_at FROM notify_deliveries WHERE ref = '77'`).Scan(&editedAt); err != nil || editedAt == nil {
		t.Fatalf("the settle edit = %v, %v", editedAt, err)
	}

	// the second pass edits and sends nothing again
	if err := sd.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(called()); n != 4 {
		t.Fatalf("the second pass called again: %d calls", n)
	}

	// a send to a chat the pass just wrote defers a second, without a try
	again := msg("sf/code", "the run finished again", MessagePoke, "T-7", when)
	queue(again, bob)
	if err := sd.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if state, tries, _ := row(bob, ""); state != "pending" || tries != 0 || next(bob).Before(time.Now()) {
		t.Fatalf("the rate row = %s %d %v", state, tries, next(bob))
	}
	if n := len(called()); n != 4 {
		t.Fatalf("the rate row sent: %d calls", n)
	}

	// unreadable writer lists hold every row back
	held := msg("sf/code", "the run finished again", MessagePoke, "T-7", when)
	queue(held, ira)
	blind := &Sender{Store: s, PublicURL: "https://console.example",
		Telegram: &Telegram{Token: "bot-key", BaseURL: api.URL},
		Lists:    func(context.Context) (string, map[string][]string, error) { return "", nil, errors.New("no lists") }}
	if err := blind.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if state, tries, _ := row(ira, ""); state != "pending" || tries != 0 || next(ira).Before(time.Now()) {
		t.Fatalf("the held row = %s %d %v", state, tries, next(ira))
	}
	if n := len(called()); n != 4 {
		t.Fatalf("the held row sent: %d calls", n)
	}

	// a sent ask whose venue left the config skips its edit; the sender
	// loop has no request boundary to catch a panic for
	karlAsk := msg("sf/code", "the card asks a question", MessageEscalation, "T-gone", when)
	queue(karlAsk, karl)
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET state = 'sent', ref = '88', sent_at = now() WHERE message_id = $1`, karlAsk); err != nil {
		t.Fatal(err)
	}
	naked := &Sender{Store: s, PublicURL: "https://console.example"}
	naked.editPass(ctx, map[string]string{"sf/code": "demo"})
	var karlEdited interface{}
	if err := s.db.QueryRowContext(ctx, `SELECT edited_at FROM notify_deliveries WHERE ref = '88'`).Scan(&karlEdited); err != nil || karlEdited != nil {
		t.Fatalf("the skipped edit = %v, %v", karlEdited, err)
	}

	// the text cut keeps whole runes
	if got := clip("hällo wörld", 5); got != "hällo" {
		t.Fatalf("clip = %q", got)
	}
}
