package factoryd

import "context"

// RoomFacts are the facts card t's room is worked out from: its record's
// (CardFacts) with its agent-time cap, the floor of its next session and
// the project's money left this month, read as tripFacts reads them. The
// next session runs on the model the card's live ask names, as Act reads
// the facts at the press (FactsAtCase). A reader that fails names itself
// in Partial; it fails only when ctx ends.
func (r *Runner) RoomFacts(ctx context.Context, t Task) (CaseFacts, error) {
	v := r.cfg.Policy()
	ask, askErr := r.store.NewestAskCase(ctx, t.ID)
	model, _ := ask.Facts["model"].(string)
	f := CardFacts(t, v, model)
	if askErr != nil {
		f.Partial = append(f.Partial, "case")
	}
	err := readFacts(ctx, &f, []factReader{
		{"config", func() error { return r.configFacts(ctx, t, &f) }},
		{"floor", func() error { return r.floorFacts(ctx, v, t, model, &f) }},
		{"month", func() error { return r.monthFacts(ctx, v, t, &f) }},
	})
	return f, err
}
