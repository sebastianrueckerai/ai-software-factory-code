package factoryd

import (
	"context"
	"sort"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The poke's messenger half (roadmap 145): a member's effective level
// joins them to a project's recipients, and the poke transaction writes
// one outbox row per connection whose level passes the row's level. A
// member of the notify list keeps their inbox row at off; an explicit row
// joins a member the notify list misses; a repeat writes nothing; a
// project with messengers false writes none; a queued row's outbox row
// follows the row's landing id.
func TestPGStorePokeDeliveries(t *testing.T) {
	dsn := schematest.DSN(t, "messenger-poke")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE messenger_codes, messenger_links, notify_levels, notify_deliveries, chat_messages, chat_conversations, tasks, events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	bind := func(login, venue, address string) {
		t.Helper()
		code, err := s.IssueMessengerCode(ctx, login)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.ConsumeMessengerCode(ctx, code, venue, address, "u-"+login); err != nil || !ok {
			t.Fatalf("bind %s: %v %v", login, ok, err)
		}
	}
	no := false
	tree := policy.Tree{Document: policy.Document{Approvers: []string{"anna"}},
		Projects: map[string]policy.ProjectConfig{
			"code":  {Repository: "sf/code", Notify: []string{"sebastian", "bob", "carla"}},
			"quiet": {Repository: "sf/quiet", Notify: []string{"sebastian"}, Messengers: &no},
		}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}

	// sebastian all on telegram and discord, bob derived needs-you on
	// telegram, carla of the notify list at off, dana all without the
	// list, eric all with no connection
	bind("sebastian", VenueTelegram, "1")
	bind("sebastian", VenueDiscord, "d1")
	bind("bob", VenueTelegram, "2")
	bind("carla", VenueTelegram, "3")
	bind("dana", VenueTelegram, "4")
	for login, level := range map[string]string{"sebastian": "all", "carla": "off", "dana": "all", "eric": "all"} {
		if err := s.SetNotifyLevel(ctx, login, "sf/code", level); err != nil {
			t.Fatal(err)
		}
	}

	deliveries := func(content string) map[string][]string {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `SELECT l.login, l.venue FROM notify_deliveries d JOIN messenger_links l ON l.id = d.link_id
			JOIN chat_messages m ON m.id = d.message_id WHERE m.content = $1 ORDER BY l.login, l.venue`, content)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][]string{}
		for rows.Next() {
			var login, venue string
			if err := rows.Scan(&login, &venue); err != nil {
				t.Fatal(err)
			}
			out[login] = append(out[login], venue)
		}
		return out
	}
	ask := Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which store?", "choices": []string{"Postgres", "SQLite"}}}
	// the first pass starts at the newest event: a primer event moves the
	// cursor there without poking
	if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-9", Kind: EventCardCommented, Actor: "sebastian", Via: ViaButton}); err != nil {
		t.Fatal(err)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{ask,
		{Repository: "sf/quiet", TaskID: "t-2", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"question": "Quiet?"}}} {
		if _, err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	got := deliveries("t-1 needs you: Which store?")
	want := map[string][]string{"sebastian": {VenueDiscord, VenueTelegram}, "bob": {VenueTelegram}, "dana": {VenueTelegram}}
	if len(got) != len(want) {
		t.Fatalf("the ask's deliveries = %v, want %v", got, want)
	}
	for login, venues := range want {
		equalStrings(t, login+"'s ask", got[login], venues)
	}
	// carla keeps her inbox row at off, eric holds no connection, the
	// quiet project writes none
	for _, login := range []string{"carla", "eric"} {
		if n, _ := s.EnsureInbox(ctx, "sf/code", login); n == 0 {
			t.Fatal("the inbox number")
		} else if rows, _ := s.ChatMessages(ctx, "sf/code", login, n, 10); len(rows) != 1 {
			t.Fatalf("%s's inbox = %+v, want the row without a delivery", login, rows)
		}
	}
	if d, _ := s.DeliveriesDue(ctx, 100); len(d) != 4 {
		t.Fatalf("the due rows = %d, want 4 (the quiet project none)", len(d))
	}

	// a repeat counts on its row and pages nobody again
	if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which store?", "choices": []string{"Postgres", "SQLite"}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.DeliveriesDue(ctx, 100); len(d) != 4 {
		t.Fatalf("after the repeat = %d rows, want the same 4", len(d))
	}

	// an inform reaches only all
	if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-1", Kind: EventAllChildrenDone, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"cards": float64(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.login FROM notify_deliveries d JOIN messenger_links l ON l.id = d.link_id
		JOIN chat_messages m ON m.id = d.message_id WHERE m.kind = $1 ORDER BY l.login, l.venue`, MessagePoke)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var informed []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			t.Fatal(err)
		}
		informed = append(informed, login)
	}
	equalStrings(t, "the inform's deliveries", informed, []string{"dana", "sebastian", "sebastian"})
	if d, _ := s.DeliveriesDue(ctx, 100); len(d) != 7 {
		t.Fatalf("after the inform = %d rows, want 7", len(d))
	}

	// a queued row's outbox row follows the row to its landing id
	line := &busyLine{busy: map[string]bool{"sebastian": true}, landed: map[string][]int64{}}
	p.Line = line
	if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-3", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Late?"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var old int64
	if err := s.db.QueryRowContext(ctx, `SELECT m.id FROM chat_messages m WHERE m.content = 't-3 needs you: Late?' AND m.login = 'sebastian'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	due := func() []Delivery {
		rows, err := s.DeliveriesDue(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	var late []Delivery
	for _, d := range due() {
		if d.Content == "t-3 needs you: Late?" {
			late = append(late, d)
		}
	}
	equalStrings(t, "the landed rows before the landing", loginsOf(late), []string{"bob", "dana"})
	for _, id := range line.landed["sebastian"] {
		if err := s.StartQueued(ctx, "sf/code", "sebastian", id); err != nil {
			t.Fatal(err)
		}
	}
	late = nil
	for _, d := range due() {
		if d.Content == "t-3 needs you: Late?" {
			late = append(late, d)
		}
	}
	equalStrings(t, "the rows after the landing", loginsOf(late), []string{"bob", "dana", "sebastian", "sebastian"})
	for _, d := range late {
		if d.MessageID == old {
			t.Fatalf("the outbox row still names the queued id %d", old)
		}
	}
}

func loginsOf(ds []Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Login
	}
	sort.Strings(out)
	return out
}
