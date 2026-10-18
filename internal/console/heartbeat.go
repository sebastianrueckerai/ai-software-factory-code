package console

import (
	"context"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// PulseState answers the heartbeat's two questions from the board the
// project agent reads (the Waker's wake): whether work sits open on p's
// board and whether a card waits on a human.
func (c *Console) PulseState(ctx context.Context, p factoryd.Project) (workOpen, waitingHuman bool, err error) {
	r, err := http.NewRequestWithContext(context.WithValue(ctx, ctxKey{}, &session{}), http.MethodGet, "/", nil)
	if err != nil {
		return false, false, err
	}
	_, d, err := c.boardText(r, p, actor{})
	if err != nil {
		return false, false, err
	}
	return d.Status.Working > 0 || d.Status.Waiting > 0, d.Status.NeedYou > 0, nil
}
