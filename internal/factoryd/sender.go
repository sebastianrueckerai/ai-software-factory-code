package factoryd

import (
	"context"
	"log"
	"slices"
	"time"
)

// The outbox's sender (roadmap 145): every pass it sends the due rows
// and edits the sent ones whose ask has settled. A row is one
// notification on one venue, one link to its card, and the settle rule
// that closes it is the same one the console shows.

// sendBackoff is the wait after each failed try; a row past the last
// fails closed.
var sendBackoff = [...]time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute}

// note logs a bookkeeping write that failed: the row keeps its state,
// and the next pass reads it again.
func (s *Sender) note(what string, err error) {
	if err != nil {
		log.Printf("messengers: %s: %v", what, err)
	}
}

// sendLimit caps one pass's sends, editWindow is how long a sent row
// stays editable, writersTTL is how long the writer lists stand, and
// startEvents bounds the events a settle derivation reads.
const (
	sendLimit   = 50
	editWindow  = 7 * 24 * time.Hour
	writersTTL  = 10 * time.Minute
	startEvents = 500
)

// Sender carries the outbox to the venues.
type Sender struct {
	Store     *PGStore
	Telegram  *Telegram
	Discord   *Discord
	PublicURL string
	// Lists names every project's writers by repository: the console's
	// own lists, one source for both halves.
	Lists func(ctx context.Context) (own string, writers map[string][]string, err error)

	lists   map[string][]string
	listsAt time.Time
	last    map[string]time.Time // a telegram chat's last send
}

// Loop runs one pass every until the context ends.
func (s *Sender) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.Pass(ctx); err != nil {
				log.Printf("messengers: the sender pass: %v", err)
			}
		}
	}
}

// Pass edits the settled rows, then sends the due ones.
func (s *Sender) Pass(ctx context.Context) error {
	ps, err := s.Store.Projects(ctx)
	if err != nil {
		return err
	}
	names := make(map[string]string, len(ps))
	for _, p := range ps {
		names[p.Repository] = p.Name
	}
	s.editPass(ctx, names)
	return s.sendPass(ctx, names)
}

// sendPass sends the due rows, one by one.
func (s *Sender) sendPass(ctx context.Context, names map[string]string) error {
	due, err := s.Store.DeliveriesDue(ctx, sendLimit)
	if err != nil {
		return err
	}
	for _, d := range due {
		s.sendOne(ctx, names, d)
	}
	return nil
}

// sendOne carries one due row to its venue, or drops it when it says
// nothing anymore.
func (s *Sender) sendOne(ctx context.Context, names map[string]string, d Delivery) {
	if AskFamily(d.message()) != "" {
		settled, err := s.settleState(ctx, d)
		if err != nil {
			s.note("retry the settled row", s.Store.RetryDelivery(ctx, d.ID, sendBackoff[0], err.Error()))
			return
		}
		if !settled.Open {
			s.note("drop the settled row", s.Store.DropDelivery(ctx, d.ID))
			return
		}
	}
	writers, err := s.writers(ctx)
	if err != nil {
		s.note("hold the row for the writer lists", s.Store.DeferDelivery(ctx, d.ID, 30*time.Second))
		return
	}
	if !slices.Contains(writers[d.Repository], d.Login) {
		log.Printf("messengers: %s writes %s no more; their level and row are gone", d.Login, d.Repository)
		s.note("drop the lost level", s.Store.DeleteNotifyLevel(ctx, d.Login, d.Repository))
		s.note("drop the unentitled row", s.Store.DropDelivery(ctx, d.ID))
		return
	}
	name := names[d.Repository]
	if name == "" {
		name = d.Repository
	}
	text := clip(name+": "+d.Content, capOf(d.Venue))
	cardURL := s.PublicURL + "/#card=" + d.TaskID
	var ref string
	switch d.Venue {
	case VenueTelegram:
		if s.Telegram == nil {
			s.note("hold the row for the unmade venue", s.Store.DeferDelivery(ctx, d.ID, time.Minute))
			return
		}
		if last, ok := s.last[d.Address]; ok && time.Since(last) < time.Second {
			s.note("hold the row for the chat's rate", s.Store.DeferDelivery(ctx, d.ID, time.Second))
			return
		}
		ref, err = s.Telegram.Send(ctx, d.Address, text, cardURL)
	case VenueDiscord:
		if s.Discord == nil {
			s.note("hold the row for the unmade venue", s.Store.DeferDelivery(ctx, d.ID, time.Minute))
			return
		}
		ref, err = s.Discord.Send(ctx, d.Address, text, cardURL)
	}
	if err == nil {
		if d.Venue == VenueTelegram {
			if s.last == nil {
				s.last = map[string]time.Time{}
			}
			s.last[d.Address] = time.Now()
		}
		s.note("mark the row sent", s.Store.RecordDeliverySent(ctx, d.ID, ref))
		return
	}
	s.sendFailure(ctx, d, err)
}

// sendFailure routes one refused send.
func (s *Sender) sendFailure(ctx context.Context, d Delivery, err error) {
	var status, retryAfter int
	switch e := err.(type) {
	case *TelegramError:
		status, retryAfter = e.Status, e.RetryAfter
	case *DiscordError:
		status, retryAfter = e.Status, e.RetryAfter
	}
	switch {
	case retryAfter > 0 || status == 429: // the venue's own rate limit: a wait, not a try
		wait := time.Duration(retryAfter) * time.Second
		if wait == 0 {
			wait = 30 * time.Second
		}
		s.note("hold the row for the venue's rate", s.Store.DeferDelivery(ctx, d.ID, wait))
	case status == 403 || status == 404: // the chat is gone
		log.Printf("messengers: %s's %s chat is gone: %v", d.Login, d.Venue, err)
		s.note("unlink the gone chat", s.Store.UnlinkMessenger(ctx, d.Login, d.Venue))
		s.note("drop the gone chat's row", s.Store.DropDelivery(ctx, d.ID))
	case status/100 == 4: // wrong in a way a retry cannot fix
		s.note("fail the row", s.Store.FailDelivery(ctx, d.ID, err.Error()))
	default: // the venue is down or busy
		if d.Tries >= len(sendBackoff) {
			s.note("fail the row", s.Store.FailDelivery(ctx, d.ID, err.Error()))
			return
		}
		s.note("retry the row", s.Store.RetryDelivery(ctx, d.ID, sendBackoff[d.Tries], err.Error()))
	}
}

// editPass writes the settle reason into the sent ask rows the settle
// rule has closed, once each.
func (s *Sender) editPass(ctx context.Context, names map[string]string) {
	sent, err := s.Store.DeliveriesSent(ctx, editWindow)
	if err != nil {
		log.Printf("messengers: the sent rows: %v", err)
		return
	}
	for _, d := range sent {
		if AskFamily(d.message()) == "" {
			continue
		}
		settled, err := s.settleState(ctx, d)
		if err != nil {
			log.Printf("messengers: the settle of message %d: %v", d.MessageID, err)
			continue
		}
		if settled.Open {
			continue
		}
		name := names[d.Repository]
		if name == "" {
			name = d.Repository
		}
		text := clip(name+": "+d.Content+" · "+ReasonLine(settled), capOf(d.Venue))
		cardURL := s.PublicURL + "/#card=" + d.TaskID
		var editErr error
		switch d.Venue {
		case VenueTelegram:
			if s.Telegram == nil { // the venue left the config; the row ages out
				continue
			}
			editErr = s.Telegram.Edit(ctx, d.Address, d.Ref, text, cardURL)
		case VenueDiscord:
			if s.Discord == nil {
				continue
			}
			editErr = s.Discord.Edit(ctx, d.Address, d.Ref, text, cardURL)
		}
		if editErr != nil {
			var status int
			switch e := editErr.(type) {
			case *TelegramError:
				status = e.Status
			case *DiscordError:
				status = e.Status
			}
			if status/100 != 4 { // another pass retries; 4xx means the message is gone
				continue
			}
		}
		s.note("mark the settle edit", s.Store.MarkDeliveryEdited(ctx, d.ID))
	}
}

// settleState derives one row's ask state from what the settle rule
// reads.
func (s *Sender) settleState(ctx context.Context, d Delivery) (PokeSettle, error) {
	var cardWaits bool
	if t, ok, err := s.Store.Task(ctx, d.TaskID); err != nil {
		return PokeSettle{}, err
	} else if ok {
		cardWaits = t.Status == "waiting-human"
	}
	events, err := s.Store.TaskEvents(ctx, d.TaskID, startEvents)
	if err != nil {
		return PokeSettle{}, err
	}
	pending, err := s.Store.ApprovalRequests(ctx, "pending")
	if err != nil {
		return PokeSettle{}, err
	}
	return SettleOf(d.message(), events, cardWaits, ApprovalWaits(pending, d.TaskID)), nil
}

// writers serves the writer lists, cached for their TTL.
func (s *Sender) writers(ctx context.Context) (map[string][]string, error) {
	if s.lists != nil && time.Since(s.listsAt) < writersTTL {
		return s.lists, nil
	}
	_, lists, err := s.Lists(ctx)
	if err != nil {
		return nil, err
	}
	s.lists, s.listsAt = lists, time.Now()
	return lists, nil
}

// message is the row's inbox half, as the settle rule reads it.
func (d Delivery) message() ChatMessage {
	return ChatMessage{Kind: d.Kind, BirthKind: d.BirthKind, TaskID: d.TaskID, At: d.At}
}

// clip cuts text to n runes without splitting one.
func clip(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n])
}

// capOf is the venue's message cap in runes.
func capOf(venue string) int {
	if venue == VenueDiscord {
		return 2000
	}
	return 4096
}
