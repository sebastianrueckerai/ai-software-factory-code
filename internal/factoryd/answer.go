package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// ChatAnswer is the direct transport's typed call: one call of alias on p,
// forcing the answer tool p's profile names, whose arguments decode into
// T, the type the step registered. deployment is the one that answered.
func ChatAnswer[T any](ctx context.Context, m *SupervisorModel, taskID, alias string, timeout time.Duration, p prompts.Prompt, input any) (out T, deployment string, err error) {
	tool, ok := p.Answer()
	switch {
	case !ok:
		return out, "", fmt.Errorf("step %s: its profile names no answer type", p.Step())
	case p.AnswerType() != reflect.TypeFor[T]():
		return out, "", fmt.Errorf("step %s answers %s, not %s", p.Step(), p.AnswerType(), reflect.TypeFor[T]())
	}
	return answerWith[T](ctx, m, taskID, alias, timeout, p, tool, input)
}

// answerWith is ChatAnswer's call forcing tool and its decode.
func answerWith[T any](ctx context.Context, m *SupervisorModel, taskID, alias string, timeout time.Duration, p prompts.Prompt, tool prompts.Tool, input any) (out T, deployment string, err error) {
	content, deployment, _, err := m.chatWith(ctx, taskID, alias, timeout, p, &tool, input)
	if err != nil {
		return out, "", err
	}
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, "", fmt.Errorf("step %s: its answer does not decode: %w", p.Step(), err)
	}
	return out, deployment, nil
}
