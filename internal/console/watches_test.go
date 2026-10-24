package console

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

// The watch tools (the concept's timers section): a thread stores its
// own subscription on a card and its one-shot timers, the card is
// resolved as every tool's card is, and a project at its cap gets the
// count named back.

// watch_card stores the thread's subscription, set_timer its timer, and
// unwatch_card ends the watch: the rows land on the calling thread.
func TestWatchToolsStoreOnTheThread(t *testing.T) {
	r, f, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "watch_card", `{"task_id":"t-m"}`, "set_timer", `{"minutes":30,"note":"Check the build."}`,
		"unwatch_card", `{"task_id":"t-m"}`)}, {say("Watch set and ended.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"watch the cache card, set a check, then stop watching"}})
	card := `#5 t-m \"Fix the cache. It drops entries.\"`
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-m"},"duration_ms":0,"name":"watch_card","result":"Watching ` + card + `: its moves wake this thread."}`,
		`tool {"arguments":{"minutes":30,"note":"Check the build."},"duration_ms":0,"name":"set_timer",` +
			`"result":"Timer set, 30 minutes from now: Check the build."}`,
		`tool {"arguments":{"task_id":"t-m"},"duration_ms":0,"name":"unwatch_card","result":"Watch ended on ` + card + `."}`,
		`text {"text":"Watch set and ended."}`,
		`done {}`})
	f.mu.Lock()
	watches, timers := len(f.watches), len(f.timers)
	due, note := f.timers[0].due, f.timers[0].note
	f.mu.Unlock()
	if watches != 0 {
		t.Fatalf("%d watches after the unwatch, want 0", watches)
	}
	if timers != 1 || note != "Check the build." {
		t.Fatalf("the stored timer = %d rows, note %q, want one with the note", timers, note)
	}
	if d := time.Until(due); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("the timer is due in %v, want 30 minutes", d)
	}
}

// A project at its cap of watches and timers gets the count named back
// as a refusal, and the minutes and the note are bounded.
func TestSetTimerBoundsAndCap(t *testing.T) {
	r, f, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	f.mu.Lock()
	for i := range 16 {
		f.watches = append(f.watches, storedWatch{repository: "sf/code", login: s.user, taskID: fmt.Sprintf("t-cap-%d", i)})
	}
	f.mu.Unlock()
	g.replies = [][]string{{call(1, "watch_card", `{"task_id":"t-m"}`, "set_timer", `{"minutes":30,"note":"No room."}`,
		"set_timer", `{"minutes":0,"note":"Too early."}`, "set_timer", `{"minutes":30,"note":""}`)}, {say("None set.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"watch the cache card and set three timers"}})
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-m"},"duration_ms":0,"name":"watch_card",` +
			`"result":"Refused: this project's watches and timers are full: it holds 16 of 16, end one first"}`,
		`tool {"arguments":{"minutes":30,"note":"No room."},"duration_ms":0,"name":"set_timer",` +
			`"result":"Refused: this project's watches and timers are full: it holds 16 of 16, end one first"}`,
		`tool {"arguments":{"minutes":0,"note":"Too early."},"duration_ms":0,"name":"set_timer",` +
			`"result":"Refused: The timer is 1 to 1440 minutes from now."}`,
		`tool {"arguments":{"minutes":30,"note":""},"duration_ms":0,"name":"set_timer",` +
			`"result":"Refused: The note is 1 to 200 characters."}`,
		`text {"text":"None set."}`,
		`done {}`})
}

// A card another member's thread already watches takes no second watch:
// the answer names the watcher, one wake turn per card is one.
func TestWatchCardNamesTheWatcher(t *testing.T) {
	r, f, g := newChatRig(t)
	f.mu.Lock()
	f.watches = append(f.watches, storedWatch{"sf/code", "alice", "t-m", 1})
	f.mu.Unlock()
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "watch_card", `{"task_id":"t-m"}`, "watch_card", `{"task_id":"#5"}`)}, {say("Twice refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"watch the cache card twice"}})
	card := `#5 t-m \"Fix the cache. It drops entries.\"`
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-m"},"duration_ms":0,"name":"watch_card","result":"` + card +
			` is already watched by alice's thread: its moves wake that thread, which you can read."}`,
		`tool {"arguments":{"task_id":"#5"},"duration_ms":0,"name":"watch_card","result":"` + card +
			` is already watched by alice's thread: its moves wake that thread, which you can read."}`,
		`text {"text":"Twice refused."}`,
		`done {}`})
}
