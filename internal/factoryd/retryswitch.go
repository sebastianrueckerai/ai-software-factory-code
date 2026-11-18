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
	// Freeze is the freeze-card watcher: its CloseOnRetry closes a
	// class's open card at once, and its policy says which cards the
	// class's hold freezes.
	Freeze *FreezeCard
	// GatewayURL and Token are the gateway's address and the
	// gateway-factoryd token its hold-clear door takes; HTTP nil =
	// http.DefaultClient.
	GatewayURL, Token string
	HTTP              *http.Client
	// Signal delivers the provider_retry signal to a task's workflow.
	Signal func(ctx context.Context, taskID string) error
}

// RetryFreeze ends class's freeze, pressed from card taskID, on whose row
// the gateway writes the class_resumed event. A hold the gateway did not
// clear moves nothing else. With no hold armed and no frozen card it is a
// quiet success.
func (s *RetrySwitch) RetryFreeze(ctx context.Context, taskID, class string) error {
	if err := s.clearHold(ctx, taskID, class); err != nil {
		return fmt.Errorf("clear the %s class's hold: %w", class, err)
	}
	return s.retryFreeze(ctx, class)
}

// retryFreeze follows a cleared hold: the hold is the class's in every
// repository, so every repository's open freeze card of the class closes,
// then every running card the hold froze is woken. The cards close before
// any probe can answer, so a probe's dated refusal re-arms the same card.
// One failed close or wake does not stop the others.
func (s *RetrySwitch) retryFreeze(ctx context.Context, class string) error {
	var errs []error
	repos, err := s.Freeze.openCardRepositories(ctx, class)
	if err != nil {
		errs = append(errs, fmt.Errorf("list the %s class's freeze cards: %w", class, err))
	}
	closed := 0
	for _, repo := range repos {
		if err := s.Freeze.CloseOnRetry(ctx, repo, class); err != nil {
			errs = append(errs, fmt.Errorf("close %s's %s freeze card: %w", repo, class, err))
			continue
		}
		closed++
	}
	cards, err := s.Freeze.classCards(ctx, class)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list the %s class's frozen cards: %w", class, err))...)
	}
	woken, frozen := 0, 0
	for _, c := range cards {
		if c.Status != "running" {
			continue
		}
		frozen++
		if err := s.Signal(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("wake %s: %w", c.ID, err))
			continue
		}
		woken++
	}
	log.Printf("retry switch: the %s class's hold cleared, %d freeze cards closed, %d of %d frozen cards woken", class, closed, woken, frozen)
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
