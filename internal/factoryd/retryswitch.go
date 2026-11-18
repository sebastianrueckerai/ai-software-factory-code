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
	Store RetrySwitchStore
	// Freeze is the freeze-card watcher, whose CloseOnRetry closes the
	// class's open card at once.
	Freeze *FreezeCard
	// GatewayURL and Token are the gateway's address and the
	// gateway-factoryd token its hold-clear door takes; HTTP nil =
	// http.DefaultClient.
	GatewayURL, Token string
	HTTP              *http.Client
	// Signal delivers the provider_retry signal to a task's workflow.
	Signal func(ctx context.Context, taskID string) error
}

// RetrySwitchStore is the store the retry switch reads.
type RetrySwitchStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	// ListFrozenCards lists the running cards of class in every
	// repository: the gateway's hold freezes the class on all of them.
	ListFrozenCards(ctx context.Context, class string) ([]Task, error)
}

// RetryFreeze ends class's freeze, pressed from card taskID: the gateway's
// class_resumed row lands on that card, and the freeze card of its
// repository closes. With no hold armed and no frozen card it is a quiet
// success.
func (s *RetrySwitch) RetryFreeze(ctx context.Context, taskID, class string) error {
	card, ok, err := s.Store.Task(ctx, taskID)
	if err != nil {
		return fmt.Errorf("read the pressing card %s: %w", taskID, err)
	}
	if !ok {
		return fmt.Errorf("no task row for %s", taskID)
	}
	return s.retryFreeze(ctx, card, class)
}

// retryFreeze clears the hold, closes the freeze card, then wakes every
// frozen card: the card is closed before any probe can answer, so a
// probe's dated refusal re-arms the same card. A hold the gateway did not
// clear wakes nothing; one card's failed wake does not stop the others.
func (s *RetrySwitch) retryFreeze(ctx context.Context, card Task, class string) error {
	if err := s.clearHold(ctx, card.ID, class); err != nil {
		return fmt.Errorf("clear the %s class's hold: %w", class, err)
	}
	var errs []error
	if err := s.Freeze.CloseOnRetry(ctx, card.Repository, class); err != nil {
		errs = append(errs, fmt.Errorf("close the %s class's freeze card: %w", class, err))
	}
	cards, err := s.Store.ListFrozenCards(ctx, class)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list the %s class's frozen cards: %w", class, err))...)
	}
	woken := 0
	for _, c := range cards {
		if err := s.Signal(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("wake %s: %w", c.ID, err))
			continue
		}
		woken++
	}
	log.Printf("retry switch: %s cleared the %s class's hold, %d of %d frozen cards woken", card.ID, class, woken, len(cards))
	return errors.Join(errs...)
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

// ListFrozenCards lists the running cards whose classes hold class, in
// every repository: a running card is in its session phase, where a
// provider-limit wait holds it.
func (s *PGStore) ListFrozenCards(ctx context.Context, class string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE status = 'running' AND required_capabilities @> jsonb_build_array($1::text) ORDER BY id`, class)
}
