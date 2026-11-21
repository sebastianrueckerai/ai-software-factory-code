package factoryd

import "context"

// RoomFacts are the facts card t's room is worked out from: its record's
// (CardFacts) with its agent-time cap, the floor of its next session and
// the project's money left this month, read as tripFacts reads them. A
// reader that fails names itself in Partial; it fails only when ctx ends.
func (r *Runner) RoomFacts(ctx context.Context, t Task) (CaseFacts, error) {
	v := r.cfg.Policy()
	f := CardFacts(t, v, "")
	err := readFacts(ctx, &f, []factReader{
		{"config", func() error { return r.configFacts(ctx, t, &f) }},
		{"floor", func() error { return r.floorFacts(ctx, v, t, "", &f) }},
		{"month", func() error { return r.monthFacts(ctx, v, t, &f) }},
	})
	return f, err
}
