package console

import (
	"context"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakeDeps is the Dependencies seam's fake: fixed rows or a fixed error.
type fakeDeps struct {
	rows []factoryd.DependencyState
	err  error
}

func (f *fakeDeps) DependencyStates(ctx context.Context) ([]factoryd.DependencyState, error) {
	return f.rows, f.err
}
