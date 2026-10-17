package factoryd

// The pulse left factoryd (card t-45cdce2626f0): the Pulser loop, the
// recompute, the PulseStore and the pulse's EnsureChatTask coupling are
// gone, and the chat task keeps its task and monthly budget on its own;
// the pulse events (pulse, pulse_computing) are no longer written, their
// history rows staying in the immutable trail. The loop's tests —
// TestPulseSchedule, TestPulseCall,
// TestPulseAsksOnceAnHourOfASpentChatBudget,
// TestPulseKeepsThePreviousOnANonJSONAnswer and
// TestPulseBacksOffAFailingGateway — went with it. The chat task they
// coupled to keeps its tests in chat_test.go, its budget-spent ask
// (EventChatBudgetSpent, the defect-b path that survives the band) in
// pokes_test.go, and the historical pulse events' place in a project's
// feed in event_store_test.go (TestPGStoreLeavesHistoricalPulseEventsOut).
