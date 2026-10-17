package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL), like TestPGStore.
func TestPGStoreMessengers(t *testing.T) {
	dsn := schematest.DSN(t, "messenger")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE messenger_codes, messenger_links, notify_levels, notify_deliveries, chat_messages, chat_conversations CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// a connect code: 22 base64url characters, bound to its login, one
	// live code per login
	code, err := s.IssueMessengerCode(ctx, "sebastian")
	if err != nil {
		t.Fatalf("IssueMessengerCode: %v", err)
	}
	if len(code) != 22 {
		t.Fatalf("the code %q is not 22 characters", code)
	}
	again, _ := s.IssueMessengerCode(ctx, "sebastian")
	if again == code {
		t.Fatal("the second code equals the first")
	}
	if _, ok, err := s.ConsumeMessengerCode(ctx, again, VenueTelegram, "4242", "7"); err != nil || !ok {
		t.Fatalf("ConsumeMessengerCode: %v %v", ok, err)
	}
	links, err := s.MessengerLinks(ctx, "sebastian")
	if err != nil || len(links) != 1 {
		t.Fatalf("MessengerLinks: %v %v", links, err)
	}
	if links[0].Venue != VenueTelegram || links[0].Address != "4242" || links[0].VenueUser != "7" {
		t.Fatalf("the link = %+v", links[0])
	}
	// a code works once and not after its ten minutes
	if _, ok, _ := s.ConsumeMessengerCode(ctx, again, VenueTelegram, "4242", "7"); ok {
		t.Fatal("the code worked twice")
	}
	late, _ := s.IssueMessengerCode(ctx, "anna")
	if _, err := s.db.ExecContext(ctx, `UPDATE messenger_codes SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ConsumeMessengerCode(ctx, late, VenueTelegram, "99", "9"); ok {
		t.Fatal("the expired code worked")
	}
	// one link per member and venue; a re-bind replaces, another member
	// stealing the same address wins it
	if _, ok, err := s.ConsumeMessengerCode(ctx, must(s.IssueMessengerCode(ctx, "sebastian")), VenueTelegram, "5151", "7"); err != nil || !ok {
		t.Fatalf("re-bind: %v %v", ok, err)
	}
	if links, _ = s.MessengerLinks(ctx, "sebastian"); len(links) != 1 || links[0].Address != "5151" {
		t.Fatalf("the re-bind left %+v", links)
	}
	if _, ok, err := s.ConsumeMessengerCode(ctx, must(s.IssueMessengerCode(ctx, "anna")), VenueTelegram, "5151", "8"); err != nil || !ok {
		t.Fatalf("anna's bind: %v %v", ok, err)
	}
	if links, _ = s.MessengerLinks(ctx, "sebastian"); len(links) != 0 {
		t.Fatalf("sebastian kept the stolen address: %+v", links)
	}
	if err := s.UnlinkMessenger(ctx, "anna", VenueTelegram); err != nil {
		t.Fatalf("UnlinkMessenger: %v", err)
	}
	if links, _ = s.MessengerLinks(ctx, "anna"); len(links) != 0 {
		t.Fatalf("the unlink left %+v", links)
	}

	// the level rows: an upsert, a read, a delete
	for _, l := range []struct{ repo, level string }{{"sf/code", "all"}, {"sf/infra", "off"}} {
		if err := s.SetNotifyLevel(ctx, "sebastian", l.repo, l.level); err != nil {
			t.Fatalf("SetNotifyLevel: %v", err)
		}
	}
	if err := s.SetNotifyLevel(ctx, "sebastian", "sf/code", "needs-you"); err != nil {
		t.Fatal(err)
	}
	levels, err := s.NotifyLevelsOf(ctx, "sebastian")
	if err != nil {
		t.Fatalf("NotifyLevelsOf: %v", err)
	}
	if levels["sf/code"] != "needs-you" || levels["sf/infra"] != "off" || len(levels) != 2 {
		t.Fatalf("the levels = %v", levels)
	}
	if err := s.DeleteNotifyLevel(ctx, "sebastian", "sf/infra"); err != nil {
		t.Fatalf("DeleteNotifyLevel: %v", err)
	}
	if levels, _ = s.NotifyLevelsOf(ctx, "sebastian"); len(levels) != 1 {
		t.Fatalf("after the delete = %v", levels)
	}

	// the outbox: a due pending row joins its link and its inbox row; a
	// sent row records the venue's message id and edits once; the try
	// bookkeeping counts and waits
	n, err := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.AddChatMessage(ctx, "sf/code", "sebastian", ChatMessage{Role: "assistant", Kind: MessageEscalation, Content: "#13 needs you", TaskID: "t1", Conversation: n, BirthKind: EventQuestionAsked})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ConsumeMessengerCode(ctx, must(s.IssueMessengerCode(ctx, "sebastian")), VenueTelegram, "4242", "7"); err != nil || !ok {
		t.Fatal("the link for the outbox")
	}
	links, _ = s.MessengerLinks(ctx, "sebastian")
	if err := s.QueueDelivery(ctx, row, links[0].ID); err != nil {
		t.Fatalf("QueueDelivery: %v", err)
	}
	due, err := s.DeliveriesDue(ctx, 10)
	if err != nil {
		t.Fatalf("DeliveriesDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	d := due[0]
	if d.MessageID != row || d.Venue != VenueTelegram || d.Address != "4242" || d.Login != "sebastian" || d.Repository != "sf/code" ||
		d.Content != "#13 needs you" || d.BirthKind != EventQuestionAsked || d.TaskID != "t1" {
		t.Fatalf("the due row = %+v", d)
	}
	if err := s.RetryDelivery(ctx, d.ID, 5*time.Minute, "429"); err != nil {
		t.Fatal(err)
	}
	if due, _ = s.DeliveriesDue(ctx, 10); len(due) != 0 {
		t.Fatalf("a retried row is due: %+v", due)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET next_try = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	due, _ = s.DeliveriesDue(ctx, 10)
	if len(due) != 1 || due[0].Tries != 1 || due[0].LastError != "429" {
		t.Fatalf("after the try = %+v", due)
	}
	if err := s.RecordDeliverySent(ctx, due[0].ID, "77"); err != nil {
		t.Fatal(err)
	}
	sent, err := s.DeliveriesSent(ctx, 7*24*time.Hour)
	if err != nil || len(sent) != 1 || sent[0].Ref != "77" {
		t.Fatalf("DeliveriesSent: %v %v", sent, err)
	}
	if err := s.MarkDeliveryEdited(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	if sent, _ = s.DeliveriesSent(ctx, 7*24*time.Hour); len(sent) != 0 {
		t.Fatalf("an edited row still lists: %+v", sent)
	}
	if err := s.FailDelivery(ctx, due[0].ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDelivery(ctx, row, links[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DropDelivery(ctx, due[0].ID+1); err != nil { // the second pending row
		t.Fatal(err)
	}
	if due, _ = s.DeliveriesDue(ctx, 10); len(due) != 0 {
		t.Fatalf("the dropped row is due: %+v", due)
	}
}
