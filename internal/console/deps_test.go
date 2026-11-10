package console

import (
	"context"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeDeps is the Dependencies seam's fake: fixed rows or a fixed error.
type fakeDeps struct {
	rows []factoryd.DependencyState
	err  error
}

func (f *fakeDeps) DependencyStates(ctx context.Context) ([]factoryd.DependencyState, error) {
	return f.rows, f.err
}

// fakeSwitches is the AliasSwitches seam's fake: fixed rows or a fixed error.
type fakeSwitches struct {
	rows []policy.AliasSwitch
	err  error
}

func (f *fakeSwitches) OpenAliasSwitches(context.Context) ([]policy.AliasSwitch, error) {
	return f.rows, f.err
}
