package console

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// watch_card, unwatch_card and set_timer (the timers concept): the
// thread itself holds its subscriptions, so its watches and timers wake
// the thread that set them, and a card's end takes its watches and bound
// timers with it. The timer is one-shot: the woken turn sets a new one
// when the thing it watches still needs it, which is the rearm.

// watchNamed is the tool's card, named as every tool names it.
func (t *turn) watchNamed(in toolInput) (factoryd.Task, string, error) {
	task, err := t.card(in)
	if err != nil {
		return task, "", err
	}
	return task, fmt.Sprintf("#%d %s %q", task.IssueNumber, task.ID, factoryd.CardTitle(task)), nil
}

// watchCard is the watch_card tool: the thread subscribes to the card's
// moves.
func (t *turn) watchCard(in toolInput) (string, error) {
	task, name, err := t.watchNamed(in)
	if err != nil {
		return "", err
	}
	if err := t.c.cfg.Chat.AddChatWatch(t.r.Context(), t.p.Repository, t.owner, t.conv, task.ID); err != nil {
		var taken *factoryd.WatchTakenError
		if errors.As(err, &taken) {
			return name + " is already watched by " + taken.Login + "'s thread: its moves wake that thread, which you can read.", nil
		}
		if errors.Is(err, factoryd.ErrChatWakeCap) {
			return "", refuse(http.StatusConflict, err.Error())
		}
		return "", refuse(http.StatusInternalServerError, "cannot watch the card")
	}
	return "Watching " + name + ": its moves wake this thread.", nil
}

// unwatchCard is the unwatch_card tool: the thread's subscription ends.
func (t *turn) unwatchCard(in toolInput) (string, error) {
	task, name, err := t.watchNamed(in)
	if err != nil {
		return "", err
	}
	ok, err := t.c.cfg.Chat.RemoveChatWatch(t.r.Context(), t.p.Repository, t.owner, task.ID)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot end the watch")
	}
	if !ok {
		return "", refuse(http.StatusNotFound, "This thread watches no card "+task.ID+".")
	}
	return "Watch ended on " + name + ".", nil
}

// setTimer is the set_timer tool: one timer, its note the wake's first
// line, bound to a card when the call names one.
func (t *turn) setTimer(in toolInput) (string, error) {
	if in.Minutes < 1 || in.Minutes > 1440 {
		return "", refuse(http.StatusBadRequest, "The timer is 1 to 1440 minutes from now.")
	}
	if n := len([]rune(in.Note)); n < 1 || n > 200 {
		return "", refuse(http.StatusBadRequest, "The note is 1 to 200 characters.")
	}
	taskID := ""
	if in.TaskID != "" {
		task, _, err := t.watchNamed(in)
		if err != nil {
			return "", err
		}
		taskID = task.ID
	}
	due := time.Now().Add(time.Duration(in.Minutes) * time.Minute)
	if err := t.c.cfg.Chat.AddChatTimer(t.r.Context(), t.p.Repository, t.owner, t.conv, due, in.Note, taskID); err != nil {
		if errors.Is(err, factoryd.ErrChatWakeCap) {
			return "", refuse(http.StatusConflict, err.Error())
		}
		return "", refuse(http.StatusInternalServerError, "cannot set the timer")
	}
	return fmt.Sprintf("Timer set, %d minutes from now: %s", in.Minutes, in.Note), nil
}
