package factoryd

import (
	"context"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// OpenAliasSwitches are the aliases the gateway now serves from their twin.
func (s *PGStore) OpenAliasSwitches(ctx context.Context) ([]policy.AliasSwitch, error) {
	return policy.OpenAliasSwitches(ctx, s.db)
}
