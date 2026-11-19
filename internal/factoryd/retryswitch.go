package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// RetrySwitch ends a provider-limit freeze early, behind both entry
// points: the chat's retry action and the freeze card's button. It makes
// no model call, so a press works whatever the provider's state: the woken
// cards' next sessions are the probes. A probe that succeeds ends the
// freeze for all; one that meets a dated refusal naming a new reset
// re-arms the gateway's hold, and the cards re-freeze at the new reset
// with no ask.
type RetrySwitch struct {
	// Freeze is the freeze-card watcher: its CloseOnRetry closes a
	// class's open card at once, its policy says which cards the class's
	// hold freezes, and its Signal wakes them.
	Freeze *FreezeCard
	// GatewayURL and Token are the gateway's address and the
	// gateway-factoryd token its hold-clear door takes; HTTP nil =
	// http.DefaultClient.
	GatewayURL, Token string
	HTTP              *http.Client
}

// errNoSignal refuses a press through a watcher wired without its Signal,
// which would clear the hold and wake no frozen card.
var errNoSignal = errors.New("the freeze card watcher has no Signal (FreezeCard.Signal): a press would wake no frozen card")

// ErrNoFreeze refuses a retry press on a card that holds no open provider
// freeze, naming the card's state and the action that fits.
type ErrNoFreeze string

func (e ErrNoFreeze) Error() string { return string(e) }

// freezeEnded is why freeze card t holds no freeze a retry ends, and what
// follows: its freeze ended and the card is done, or its class resumed on
// its own and the card closes itself; "" while it holds.
func freezeEnded(t Task) string {
	switch {
	case t.Status != "waiting-human":
		return fmt.Sprintf("the freeze of %s ended, the card is %s: no hold waits on a retry, and a new limit on the class opens a freeze card again", cardName(t), t.Status)
	case strings.Contains(t.Objective, freezeResumedSentence):
		closes := "itself"
		if at, ok := freezeResumeStamp(t.Objective); ok {
			closes += " at " + stamp(at)
		}
		return fmt.Sprintf("the %s class resumed on its own and %s closes %s: no hold waits on a retry, its frozen cards wake once the resume holds five minutes, and a new limit opens a freeze card again",
			freezeCardClass(t.Objective), cardName(t), closes)
	}
	return ""
}

// PartialPressError is a press that cleared the hold and then failed part
// of what follows: Err joins the closes and the wakes that failed.
type PartialPressError struct{ Err error }

func (e *PartialPressError) Error() string { return "the hold cleared, but " + e.Err.Error() }
func (e *PartialPressError) Unwrap() error { return e.Err }

// RetryFreeze is the press on freeze card cardID, by a human or the
// project agent, reaching the factory via (ViaButton, ViaChat): it ends the
// freeze of the class on the card's face, the gateway writing the
// class_resumed event on the card's row. It reads the frozen cards first,
// so a read that fails moves nothing, and a hold the gateway did not clear
// moves nothing else; once the hold cleared, the press runs to its end
// whatever becomes of the caller. With no hold armed and no frozen card it
// is a quiet success.
func (s *RetrySwitch) RetryFreeze(ctx context.Context, cardID, by, via string) error {
	card, ok, err := s.Freeze.Store.Task(ctx, cardID)
	if err != nil {
		return fmt.Errorf("read card %s: %w", cardID, err)
	}
	class := freezeCardClass(card.Objective)
	switch {
	case !ok:
		return ErrNoFreeze("no card " + cardID + " stands: the retry switch ends a provider freeze from the class's open freeze card")
	case card.Source != SourceFreezeCard || class == "":
		return ErrNoFreeze(fmt.Sprintf("%s is no freeze card: the retry switch ends a provider freeze from the class's open freeze card, and Retry files a failed card again", cardName(card)))
	case !FreezeCardHolds(card):
		return ErrNoFreeze(freezeEnded(card))
	case s.Freeze.Signal == nil:
		return errNoSignal
	}
	cards, err := s.Freeze.classCards(ctx, class)
	if err != nil {
		return fmt.Errorf("list the %s class's frozen cards: %w", class, err)
	}
	if err := s.clearHold(ctx, cardID, class); err != nil {
		return fmt.Errorf("clear the %s class's hold: %w", class, err)
	}
	return s.retryFreeze(context.WithoutCancel(ctx), cardID, class, by, via, cards)
}

// retryFreeze follows a cleared hold: the hold is the class's in every
// repository, so every repository's open freeze card of the class closes,
// then every running card of cards, the ones the hold froze, is woken.
// The cards close before any probe can answer, so a probe's dated refusal
// re-arms the same card. One failed close or wake does not stop the others,
// and the failures come back as a PartialPressError. The press is recorded
// on card cardID, the one pressed (EventFreezeRetried).
func (s *RetrySwitch) retryFreeze(ctx context.Context, cardID, class, by, via string, cards []Task) error {
	var errs []error
	repos, err := s.Freeze.openCardRepositories(ctx, class)
	if err != nil {
		errs = append(errs, fmt.Errorf("list the %s class's freeze cards: %w", class, err))
	}
	closed := 0
	for _, repo := range repos {
		if err := s.Freeze.CloseOnRetry(ctx, repo, class, by); err != nil {
			errs = append(errs, fmt.Errorf("close %s's %s freeze card: %w", repo, class, err))
			continue
		}
		closed++
	}
	woken, frozen, failed := s.Freeze.wake(ctx, cards)
	errs = append(errs, failed...)
	log.Printf("retry switch: %s cleared the %s class's hold, %d freeze cards closed, %d of %d frozen cards woken", by, class, closed, woken, frozen)
	s.Freeze.Timeline.RecordRow(ctx, cardID, EventFreezeRetried, by, via, map[string]any{"class": class, "closed": closed, "woken": woken, "frozen": frozen})
	if len(errs) > 0 {
		return &PartialPressError{Err: errors.Join(errs...)}
	}
	return nil
}

// clearHold posts the class to the gateway's hold-clear door on taskID's
// account.
func (s *RetrySwitch) clearHold(ctx context.Context, taskID, class string) error {
	body, err := json.Marshal(struct {
		Class string `json:"class"`
	}{class})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.GatewayURL+"/v1/hold/clear", bytes.NewReader(body)) // #nosec G704 -- the host is the configured GatewayURL
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req) // #nosec G704 -- the request built above, to GatewayURL
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return &StatusError{Code: res.StatusCode, Body: textutil.Truncate(string(bytes.TrimSpace(raw)), 300)}
	}
	return nil
}
