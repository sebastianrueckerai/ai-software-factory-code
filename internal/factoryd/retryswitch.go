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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// RetrySwitch is the retry switch: the one mechanism behind its two entry
// points, the chat's retry card action and the freeze card's retry button.
// RetryFreeze makes no model call itself, so the press works whatever the
// provider's state: it ends the class's hold at the gateway, wakes every
// frozen card's workflow (SignalProviderRetry) — the woken cards' next
// sessions are the probes — and closes the class's freeze card at once
// (FreezeCard.CloseOnRetry). A probe that succeeds ends the freeze for
// all; a probe that meets a dated refusal naming a new reset re-arms the
// hold, a fresh class_frozen row, and the cards re-freeze at the new
// reset with no ask. With no hold armed and no frozen cards the press is
// a no-op success.
type RetrySwitch struct {
	// Store reads the pressing card and lists the class's frozen cards.
	Store RetrySwitchStore
	// Freeze is the freeze-card watcher: the press closes the class's
	// open card at once.
	Freeze *FreezeCard
	// GatewayURL and Token are the gateway's address and the
	// gateway-factoryd token its hold-clear door takes; HTTP is nil =
	// http.DefaultClient.
	GatewayURL, Token string
	HTTP              *http.Client
	// Signal delivers the provider_retry signal to a task's workflow, as
	// the cap waits' wake delivers the policy load.
	Signal func(ctx context.Context, taskID string) error
}

// RetrySwitchStore is the store the retry switch reads.
type RetrySwitchStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	// ListFrozenCards lists the class's frozen cards on the repository:
	// the cards whose workflow holds a provider-limit wait, FrozenCards'
	// count in full.
	ListFrozenCards(ctx context.Context, repository, class string) ([]Task, error)
}

// holdClearTimeout bounds the press's call to the gateway.
const holdClearTimeout = 10 * time.Second

// RetryFreeze ends class's freeze from the pressing card task: the
// contract both entry points call. taskID names the pressing card, whose
// repository the freeze covers and whose row the gateway's class_resumed
// event lands on.
func (s *RetrySwitch) RetryFreeze(ctx context.Context, taskID, class string) error {
	return s.retryFreeze(ctx, taskID, class)
}

// retryFreeze clears the class's hold at the gateway, wakes every frozen
// card's workflow and closes the freeze card at once, in that order: the
// hold is gone before a woken card's session can probe, and the card is
// closed before the probe's answer arrives — a first success keeps it
// closed, a first dated refusal re-arms the hold and updates the same
// card. One card's wake failing does not stop the others; the failures
// come back joined.
func (s *RetrySwitch) retryFreeze(ctx context.Context, taskID, class string) error {
	t, ok, err := s.Store.Task(ctx, taskID)
	if err != nil {
		return fmt.Errorf("read the pressing card %s: %w", taskID, err)
	}
	if !ok {
		return fmt.Errorf("no task row for %s", taskID)
	}
	if err := s.clearHold(ctx, taskID, class); err != nil {
		return fmt.Errorf("clear the %s class's hold: %w", class, err)
	}
	cards, err := s.Store.ListFrozenCards(ctx, t.Repository, class)
	if err != nil {
		return fmt.Errorf("list the %s class's frozen cards: %w", class, err)
	}
	var errs []error
	woken := 0
	for _, c := range cards {
		if err := s.Signal(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("wake %s: %w", c.ID, err))
			continue
		}
		woken++
	}
	if err := s.Freeze.CloseOnRetry(ctx, t.Repository, class); err != nil {
		errs = append(errs, fmt.Errorf("close the %s class's freeze card: %w", class, err))
	}
	log.Printf("retry switch: the %s class's hold on %s cleared, %d of %d cards woken, the freeze card closed at once", class, t.Repository, woken, len(cards))
	return errors.Join(errs...)
}

// clearHold ends the class's hold at the gateway's hold-clear door
// (POST /v1/hold/clear) on the pressing card's account: the factoryd
// token in the bearer header, the task and the purpose in the factory
// headers, as the window read names its identity. A class with no armed
// hold answers a quiet success.
func (s *RetrySwitch) clearHold(ctx context.Context, taskID, class string) error {
	body, err := json.Marshal(map[string]string{"class": class})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, holdClearTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.GatewayURL+"/v1/hold/clear", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	req.Header.Set(gateway.HeaderFactoryPurpose, policy.PurposeOperator)
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req) // #nosec G204 -- the host is the configured GatewayURL, a variable only in tests
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", res.StatusCode, textutil.Truncate(string(bytes.TrimSpace(raw)), 300))
	}
	var cleared struct {
		Class string `json:"class"`
	}
	return json.Unmarshal(raw, &cleared)
}

// ListFrozenCards lists the class's frozen cards: the repository's cards
// whose model alias is the class and that hold a work slot or wait for
// one, the cards the freeze covers (FrozenCards' count in full).
func (s *PGStore) ListFrozenCards(ctx context.Context, repository, class string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE repository = $1 AND model_alias = $2 AND status IN ('running', 'waiting', 'queued') ORDER BY id`, repository, class)
}
