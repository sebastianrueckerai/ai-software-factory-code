package factoryd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
	if err := os.MkdirAll(o.WorkDir, 0o750); err != nil {
		return err
	}
	briefJSON, err := json.Marshal(brief)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.WorkDir, "brief.json"), briefJSON, 0o600)
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
	proxy := &http.Server{Handler: &IdentityProxy{GatewayURL: o.GatewayURL, TokenFile: o.TokenFile, HTTP: o.HTTP}, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		if err := proxy.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("opencode: identity proxy: %v", err)
		}
		close(done)
	}()
	defer func() { proxy.Shutdown(context.Background()); <-done }() // #nosec G104 -- shutdown error is inconsequential once the run is over

	provider, modelID := o.model()
	cfg, err := buildOpencodeConfig("http://"+ln.Addr().String(), provider, modelID, outputLimit)
	if err != nil {
		return o.finish(emit, false, err.Error())
	}

	p := &opencodeParser{workDir: o.WorkDir}
	lines := &lineWriter{
		line:    func(b []byte) { p.line(b, emit) },
		tooLong: func() { p.fail("opencode output line exceeds 1 MiB") },
	}
	cmd := exec.CommandContext(ctx, o.binary(),
		"run", "--format", "json", "--auto", "--model", provider+"/"+modelID, objectiveText(brief)) // #nosec G204 -- exec.CommandContext, no shell; objectiveText(brief) is one argv element from factoryd's own task brief
	cmd.Dir = o.WorkDir
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG_CONTENT="+string(cfg))
	cmd.Stdout = lines
	stderr := &capWriter{max: 64 << 10}
	cmd.Stderr = stderr
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
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // #nosec G104 -- best-effort reap of a process group already exited; ESRCH is expected
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

// maxOutputLine bounds one opencode JSON line, like the gateway's SSE line.
const maxOutputLine = 1 << 20

// lineWriter hands each complete line to line; flush parses a final line
// that lacks its newline. A line past maxOutputLine is dropped up to its
// newline, unbuffered, and reported once to tooLong.
type lineWriter struct {
	buf      []byte
	skipping bool
	line     func([]byte)
	tooLong  func()
}

func (w *lineWriter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if !w.skipping && len(w.buf)+len(part) > maxOutputLine {
			w.skipping, w.buf = true, nil
			w.tooLong()
		}
		if !w.skipping {
			w.buf = append(w.buf, part...)
		}
		if i < 0 {
			break
		}
		if !w.skipping {
			w.flush()
		}
		w.skipping, w.buf = false, nil
		b = b[i+1:]
	}
	return n, nil
}

func (w *lineWriter) flush() {
	if l := bytes.TrimSpace(w.buf); len(l) > 0 {
		w.line(l)
	}
	w.buf = nil
}

// capWriter keeps the first max bytes written to it and drops the rest.
type capWriter struct {
	bytes.Buffer
	max int
}

func (w *capWriter) Write(b []byte) (int, error) {
	w.Buffer.Write(b[:min(len(b), max(0, w.max-w.Len()))])
	return len(b), nil
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
	if len(brief.Answers) > 0 {
		b.WriteString("The human has answered this task's escalations (newest first). These answers settle those questions: act on them and do not escalate them again.\n")
		for _, a := range brief.Answers {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
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

// mcpTimeoutMS is opencode's per-request timeout on the gateway's MCP
// tools, which also cuts a tool call (5000 ms by default): above the
// gateway's slowest connector, fal's 120 s client timeout (Replicate:
// a 60 s wait, a cancel and a read within 75 s).
const mcpTimeoutMS = 150000

// buildOpencodeConfig produces the pinned v1.18.32 config for the identity
// proxy at proxyURL (provider singular, options.baseURL under the provider
// block; the current docs' top-level baseURL shape is the v2 line and
// yields "undefined/chat/completions" at this version), with the gateway's
// MCP tools as the remote server factory-tools: the proxy attaches the
// task identity, so the entry carries no header. The model sees the tools
// as factory-tools_<tool>.
func buildOpencodeConfig(proxyURL, provider, modelID string, outputLimit int) ([]byte, error) {
	baseURL := proxyURL + "/v1"
	cfg := map[string]any{
		"mcp": map[string]any{
			"factory-tools": map[string]any{
				"type": "remote", "url": proxyURL + "/mcp", "enabled": true, "oauth": false, "timeout": mcpTimeoutMS,
			},
		},
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
	workDir     string // relative file paths resolve here for the content hash
	pendingText string
	lastText    string
	stopped     bool
	errMsg      string
}

func (p *opencodeParser) errMessage() string { return p.errMsg }

func (p *opencodeParser) fail(msg string) {
	if p.errMsg == "" {
		p.errMsg = msg
	}
}

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
			Output   string         `json:"output"`
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
			"tool":        part.Tool,
			"status":      part.State.Status,
			"error":       part.State.Error,
			"args":        capArgs(part.State.Input),
			"args_sha256": argsHash(part.State.Input),
		}})
		return
	}
	switch part.Tool {
	case "bash":
		detail := map[string]any{"command": part.State.Input["command"], "status": "completed", "output": outputTail(part.State.Output)}
		if part.State.Metadata.Exit != nil {
			detail["exit"] = *part.State.Metadata.Exit
		}
		emit(harness.Event{Time: at, Kind: harness.EventCommand, Detail: detail})
	case "write", "edit", "patch":
		detail := map[string]any{"path": part.State.Input["filePath"]}
		if path, ok := part.State.Input["filePath"].(string); ok && path != "" {
			if !filepath.IsAbs(path) {
				path = filepath.Join(p.workDir, path)
			}
			detail["sha256"] = fileSHA256(path)
		}
		emit(harness.Event{Time: at, Kind: harness.EventFileChange, Detail: detail})
	default:
		emit(harness.Event{Time: at, Kind: harness.EventToolCall, Detail: map[string]any{
			"tool":        part.Tool,
			"status":      "completed",
			"args":        capArgs(part.State.Input),
			"args_sha256": argsHash(part.State.Input),
		}})
	}
}

const (
	maxOutputTail = 4 << 10  // bytes of a command's output an event carries
	maxArgString  = 512      // bytes of one string argument an event carries
	maxHashedFile = 64 << 20 // larger files get no content hash
)

// outputTail keeps the end of a command's output, cut at a line start.
func outputTail(out string) string {
	if len(out) <= maxOutputTail {
		return out
	}
	out = out[len(out)-maxOutputTail:]
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	return out
}

// capArgs copies a tool's arguments with long strings cut; args_sha256
// identifies the whole call.
func capArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			v = textutil.Truncate(s, maxArgString)
		}
		out[k] = v
	}
	return out
}

// fileSHA256 hashes a regular file's content. The agent controls the path,
// so a symlink, fifo or device yields no hash instead of a read through it
// or a blocked open.
func fileSHA256(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- only hashed, never returned; non-regular files are refused below
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxHashedFile {
		return ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxHashedFile)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
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
