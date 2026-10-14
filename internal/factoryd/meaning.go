package factoryd

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/manual"

// The statuses and gates and their meanings live in internal/manual, the
// one source the gateway's manual tool and the workers' basics read too.
var (
	FinalStatuses = manual.FinalStatuses
	Statuses      = manual.Statuses
)

func StatusMeaning(status string) string { return manual.StatusMeaning(status) }

func GateMeaning(gate string) string { return manual.GateMeaning(gate) }
