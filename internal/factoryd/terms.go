package factoryd

// Terms are the factory's own words a face may use, each with the one
// plain sentence that tells a reader who dropped in cold what it is. No
// sentence leans on another term.
var Terms = map[string]string{
	"session":         "A session is one run of an AI agent on a card, until it hands in its change or stops.",
	"criterion":       "A criterion is one promise of a plan, written so that a test, an AI model or a human can check the work against it.",
	"hold-out":        "A hold-out is an extra promise that an AI review proposes for a plan, checked by a test the building agents see only once it fails.",
	"check-in":        "A check-in is an AI step that weighs a card's open findings before its agent runs again, and stops the card when only a human can act.",
	"spend limit":     "The spend limit is the most money a card may get, so adding money cannot take its budget past it, and a human can raise it.",
	"successor":       "A successor is a new card that takes over a stuck card's work, with its code so far, its open findings and a budget of its own.",
	"replan":          "A replan sends a card or a plan back to the planner, the AI step that cuts the work into cards again.",
	"proof":           "A proof is the test of one promise of a plan, written by a separate AI agent and hidden from the agents that build until it fails.",
	"prover":          "The prover is the AI agent that writes the hidden test of each promise of a plan.",
	"plan round":      "A plan round is one batch of a plan's cards, and the next batch is planned once this one has merged.",
	"merge approval":  "A merge approval is a human's yes to a checked pull request before it goes into main.",
	"session cap":     "The session cap is how many times AI agents may run on a card, and more runs need a grant from the monthly pool or a human.",
	"agent time":      "Agent time is how long AI agents may work on a card in total, not counting waits for checks or reviews.",
	"session floor":   "The session floor is the least free money a card must hold before an AI agent may start a new run on it.",
	"month cap":       "The month cap is the most money a project, or the whole factory, may spend on AI models in one calendar month.",
	"line bound":      "The line bound is the most lines a card's change may add and remove together, and a larger push is refused.",
	"gate":            "A gate is a check a pull request must pass before it merges, such as the tests, the review or a human's yes.",
	"chain":           "The chain is the run of checks and deploys on main that takes merged work live.",
	"monitor":         "The monitor is a check without AI that reads each card's record and flags a card whose record looks wrong.",
	"gateway":         "The gateway is the one door for every AI model call, and it counts the money and stops calls past a limit.",
	"sweep":           "The sweep is a check without AI that finds cards quiet for half an hour and starts their stopped runs again.",
	"merge queue":     "The merge queue merges one pull request of a project at a time, oldest first, and tests each again on the newest main.",
	"roll":            "A roll is a change the factory makes itself that moves its own running services to the newest build.",
	"audit":           "The audit is an AI security check of a pull request against a fixed list of risks, and a human reads what it finds.",
	"day cap":         "The day cap is the most money the whole factory may spend on AI models in one day.",
	"escalation pool": "The escalation pool is the extra runs, work time and money per project and month the factory may grant a stuck card without a human.",
}
