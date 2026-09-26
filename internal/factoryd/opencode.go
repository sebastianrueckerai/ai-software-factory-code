package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// TokenBudget is the caller-side answer-token budget for one task. GLM is
// reasoning-always-on (200+ reasoning tokens even for trivial prompts), so
// the completion output limit must be answer plus explicit headroom or the
// answer truncates mid-flight (Stage 5 gate item).
type TokenBudget struct {
	AnswerTokens   int
	HeadroomTokens int
}

func (b TokenBudget) OutputLimit() (int, error) {
	if b.AnswerTokens <= 0 {
		return 0, fmt.Errorf("token budget: AnswerTokens must be positive, is %d", b.AnswerTokens)
	}
	if b.HeadroomTokens <= 0 {
		return 0, fmt.Errorf("token budget: HeadroomTokens must be positive (GLM always reasons), is %d", b.HeadroomTokens)
	}
	return b.AnswerTokens + b.HeadroomTokens, nil
}

// Opencode adapts the pinned opencode v1.18.32 CLI to harness.Harness. It
// runs the identity proxy on an ephemeral localhost port and points the
// provider config at it, so the only credential the harness ever holds is
// the dummy key the proxy replaces (architecture 6.3).
type Opencode struct {
	Binary     string // default "opencode"
	WorkDir    string // sandbox workspace; brief.json lands here
	GatewayURL string // the proxy forwards model calls here
	TokenFile  string // projected ServiceAccount JWT, re-read per request
	Model      string // default "factory/factory-main"
	Budget     TokenBudget
	HTTP       *http.Client
}

var _ harness.Harness = (*Opencode)(nil)

func (o *Opencode) binary() string {
	if o.Binary != "" {
		return o.Binary
	}
	return "opencode"
}

func (o *Opencode) model() (provider, modelID string) {
	m := o.Model
	if m == "" {
		m = "factory/factory-main"
	}
	if i := strings.Index(m, "/"); i >= 0 {
		return m[:i], m[i+1:]
	}
	return "factory", m
}

// Prepare validates the budget (a headroomless budget must fail the task,
// not truncate it mid-run) and materializes the brief in the workspace.
func (o *Opencode) Prepare(ctx context.Context, env harness.SandboxEnv, brief harness.TaskBrief) error {
	if _, err := o.Budget.OutputLimit(); err != nil {
		return err
	}
	if o.WorkDir == "" {
		return fmt.Errorf("opencode: WorkDir is required")
	}
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return err
	}
	briefJSON, err := json.Marshal(brief)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.WorkDir, "brief.json"), briefJSON, 0o644)
}

// Run drives `opencode run --format json` and normalizes its JSONL stream
// onto harness events. The returned Result is the adapter's verdict; the
// final finished event is synthesized here because the CLI has no such line.
func (o *Opencode) Run(ctx context.Context, brief harness.TaskBrief, emit func(harness.Event)) harness.Result {
	outputLimit, err := o.Budget.OutputLimit()
	if err != nil {
		return o.finish(emit, false, err.Error())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return o.finish(emit, false, "identity proxy listen: "+err.Error())
	}
	proxy := &http.Server{Handler: &IdentityProxy{GatewayURL: o.GatewayURL, TokenFile: o.TokenFile, HTTP: o.HTTP}}
	done := make(chan struct{})
	go func() { proxy.Serve(ln); close(done) }()
	defer func() { proxy.Shutdown(context.Background()); <-done }()

	provider, modelID := o.model()
	cfg, err := buildOpencodeConfig("http://"+ln.Addr().String()+"/v1", provider, modelID, outputLimit)
	if err != nil {
		return o.finish(emit, false, err.Error())
	}

	p := &opencodeParser{}
	lines := &lineWriter{line: func(b []byte) { p.line(b, emit) }}
	cmd := exec.CommandContext(ctx, o.binary(),
		"run", "--format", "json", "--auto", "--model", provider+"/"+modelID, objectiveText(brief))
	cmd.Dir = o.WorkDir
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG_CONTENT="+string(cfg))
	cmd.Stdout = lines
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// opencode and everything the agent starts share one process group, so
	// nothing it leaves behind outlives the session; a leftover holding the
	// output pipe gets WaitDelay before the group dies.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return o.finish(emit, false, err.Error())
	}
	waitErr := cmd.Wait()
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}
	lines.flush()

	success := p.stopped && p.errMessage() == "" && waitErr == nil
	summary := p.summary()
	if msg := p.errMessage(); msg != "" {
		summary = msg
	} else if waitErr != nil {
		summary = fmt.Sprintf("opencode exited: %v; stderr: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return o.finish(emit, success, summary)
}

// lineWriter hands each complete line to line; flush parses a final line
// that lacks its newline.
type lineWriter struct {
	buf  []byte
	line func([]byte)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		if l := bytes.TrimSpace(w.buf[:i]); len(l) > 0 {
			w.line(l)
		}
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	if l := bytes.TrimSpace(w.buf); len(l) > 0 {
		w.line(l)
	}
	w.buf = nil
}

func (o *Opencode) finish(emit func(harness.Event), success bool, summary string) harness.Result {
	if emit != nil {
		emit(harness.Event{
			Time:   time.Now(),
			Kind:   harness.EventFinished,
			Detail: map[string]any{"success": success, "summary": summary},
		})
	}
	return harness.Result{Success: success, Summary: summary}
}

func objectiveText(brief harness.TaskBrief) string {
	var b strings.Builder
	b.WriteString(brief.Objective)
	if brief.Repository != "" {
		fmt.Fprintf(&b, "\n\nRepository: %s", brief.Repository)
		if brief.SHA != "" {
			fmt.Fprintf(&b, " at %s", brief.SHA)
		}
	}
	if brief.IssueURL != "" {
		fmt.Fprintf(&b, "\nIssue: %s", brief.IssueURL)
	}
	if len(brief.Findings) > 0 {
		b.WriteString("\nFindings from earlier attempts:")
		for _, f := range brief.Findings {
			fmt.Fprintf(&b, "\n- %s", f)
		}
	}
	return b.String()
}

// buildOpencodeConfig produces the pinned v1.18.32 config (provider
// singular, options.baseURL under the provider block; the current docs'
// top-level baseURL shape is the v2 line and yields
// "undefined/chat/completions" at this version).
func buildOpencodeConfig(baseURL, provider, modelID string, outputLimit int) ([]byte, error) {
	cfg := map[string]any{
		"provider": map[string]any{
			provider: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Factory gateway",
				"options": map[string]any{
					"baseURL": baseURL,
					// The identity proxy replaces this on every request.
					"apiKey": "sandbox-dummy",
				},
				"models": map[string]any{
					modelID: map[string]any{
						"name":      "Factory model",
						"reasoning": true,
						"interleaved": map[string]any{
							"field": "reasoning_content",
						},
						"limit": map[string]any{
							"context": 200000,
							"output":  outputLimit,
						},
					},
				},
			},
		},
	}
	return json.Marshal(cfg)
}

// opencodeParser normalizes one opencode run's JSONL stream. Line shapes are
// the ones recorded from the pinned binary in testdata/.
type opencodeParser struct {
	pendingText string
	lastText    string
	stopped     bool
	errMsg      string
}

func (p *opencodeParser) errMessage() string { return p.errMsg }

func (p *opencodeParser) summary() string { return p.lastText }

func (p *opencodeParser) line(b []byte, emit func(harness.Event)) {
	if p.errMsg != "" {
		return
	}
	var raw struct {
		Type      string          `json:"type"`
		Timestamp int64           `json:"timestamp"`
		Part      json.RawMessage `json:"part"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		p.errMsg = fmt.Sprintf("unparseable opencode line: %s", b)
		return
	}
	at := time.Time{}
	if raw.Timestamp > 0 {
		at = time.UnixMilli(raw.Timestamp)
	}
	switch raw.Type {
	case "step_start":
		emit(harness.Event{Time: at, Kind: harness.EventCheckpoint})
	case "tool_use":
		p.toolUse(raw.Part, at, emit)
	case "text":
		var part struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw.Part, &part); err == nil {
			p.pendingText += part.Text
		}
	case "step_finish":
		p.stepFinish(raw.Part, at, emit)
	case "error":
		var e struct {
			Name string `json:"name"`
			Data struct {
				Message    string  `json:"message"`
				StatusCode float64 `json:"statusCode"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw.Error, &e); err != nil {
			p.errMsg = fmt.Sprintf("unparseable opencode error: %s", raw.Error)
			return
		}
		p.errMsg = fmt.Sprintf("%s: %s (status %d)", e.Name, e.Data.Message, int(e.Data.StatusCode))
	}
}

func (p *opencodeParser) toolUse(partJSON []byte, at time.Time, emit func(harness.Event)) {
	var part struct {
		Tool  string `json:"tool"`
		State struct {
			Status   string         `json:"status"`
			Input    map[string]any `json:"input"`
			Error    string         `json:"error"`
			Metadata struct {
				Exit *int `json:"exit"`
			} `json:"metadata"`
		} `json:"state"`
	}
	if err := json.Unmarshal(partJSON, &part); err != nil {
		p.errMsg = fmt.Sprintf("unparseable tool_use: %s", partJSON)
		return
	}
	if part.State.Status != "completed" {
		emit(harness.Event{Time: at, Kind: harness.EventToolCall, Detail: map[string]any{
			"tool":   part.Tool,
			"status": part.State.Status,
			"error":  part.State.Error,
		}})
		return
	}
	switch part.Tool {
	case "bash":
		detail := map[string]any{"command": part.State.Input["command"], "status": "completed"}
		if part.State.Metadata.Exit != nil {
			detail["exit"] = *part.State.Metadata.Exit
		}
		emit(harness.Event{Time: at, Kind: harness.EventCommand, Detail: detail})
	case "write", "edit", "patch":
		emit(harness.Event{Time: at, Kind: harness.EventFileChange, Detail: map[string]any{
			"path": part.State.Input["filePath"],
		}})
	default:
		emit(harness.Event{Time: at, Kind: harness.EventToolCall, Detail: map[string]any{
			"tool":   part.Tool,
			"status": "completed",
		}})
	}
}

func (p *opencodeParser) stepFinish(partJSON []byte, at time.Time, emit func(harness.Event)) {
	var part struct {
		Reason string `json:"reason"`
		Tokens struct {
			Total     float64 `json:"total"`
			Input     float64 `json:"input"`
			Output    float64 `json:"output"`
			Reasoning float64 `json:"reasoning"`
		} `json:"tokens"`
		Cost float64 `json:"cost"`
	}
	if err := json.Unmarshal(partJSON, &part); err != nil {
		p.errMsg = fmt.Sprintf("unparseable step_finish: %s", partJSON)
		return
	}
	detail := map[string]any{
		"reason": part.Reason,
		"tokens": map[string]any{
			"total":     part.Tokens.Total,
			"input":     part.Tokens.Input,
			"output":    part.Tokens.Output,
			"reasoning": part.Tokens.Reasoning,
		},
		"cost": part.Cost,
	}
	if p.pendingText != "" {
		detail["text"] = p.pendingText
		p.lastText = p.pendingText
		p.pendingText = ""
	}
	if part.Reason == "stop" {
		p.stopped = true
	}
	emit(harness.Event{Time: at, Kind: harness.EventModelCall, Detail: detail})
}
