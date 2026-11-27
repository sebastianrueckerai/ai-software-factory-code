package factoryd

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"

func init() {
	for name, n := range map[string]int{
		"maxReviewSummary":  maxReviewSummary,
		"maxReviewFindings": maxReviewFindings,
		"maxReviewFinding":  maxReviewFinding,
		"maxPreCheck":       maxPreCheck,
		"maxAuditField":     maxAuditField,
		"maxAuditWhy":       maxAuditWhy,
		"maxGateText":       maxGateText,
		"writerHappenedMax": writerHappenedMax,
		"writerLeftMax":     writerLeftMax,
		"briefChars":        briefChars,
		"detailChars":       detailChars,
		"planNameMax":       planNameMax,
		"maxHoldouts":       maxHoldouts,
		"maxHoldoutText":    maxHoldoutText,
		"maxScreenReasons":  maxScreenReasons,
		"maxScreenReason":   maxScreenReason,
	} {
		prompts.RegisterLimit(name, n)
	}
	prompts.RegisterAnswer[ReviewAnswer]("review")
	prompts.RegisterAnswer[AuditAnswer]("audit")
	prompts.RegisterAnswer[GateDecisionAnswer]("gate-decision")
	prompts.RegisterAnswer[DeltaAnswer]("delta")
	prompts.RegisterAnswer[HoldoutAnswer]("holdouts")
	prompts.RegisterAnswer[JudgementAnswer]("judgement")
	prompts.RegisterAnswer[ScreenVerdict]("answer-screening")
	prompts.RegisterAnswer[BriefAnswer]("brief")
	prompts.RegisterAnswer[CaseBriefAnswer]("case-brief")
	prompts.RegisterAnswer[NameAnswer]("plan-name")
	prompts.RegisterAnswer[LivenessAnswer]("liveness")
}

// ReviewAnswer is the model review's answer. The parser decodes Review,
// which holds these fields and the review's record.
type ReviewAnswer struct {
	Verdict           string   `json:"verdict" enum:"approve,concerns" desc:"approve when you would merge it as it is, concerns otherwise"`
	Summary           string   `json:"summary" max:"maxReviewSummary" desc:"what the change does and what you found"`
	Findings          []string `json:"findings" max:"maxReviewFindings" item:"maxReviewFinding" desc:"each one concrete issue naming the file; empty for approve without remarks"`
	StrictImprovement bool     `json:"strict_improvement" desc:"true only when the change fixes a real fault and is a strict improvement with no drawback (nothing anyone could rely on gets worse)"`
	Claims            []Claim  `json:"claims,omitempty" desc:"one for every finding that says a name is absent, undefined, never called or not implemented, or that a named Go test passes or fails at the base or at the head; the factory searches the repository at the head and at main for a symbol, runs a test at its ref, and drops a finding the search or the run contradicts; none for any other finding"`
}

// AuditAnswer is the security audit's answer.
type AuditAnswer struct {
	Findings []AuditAnswerFinding `json:"findings" max:"maxPreCheck" desc:"every change that touches a checklist item, even one that looks intended, and nothing else; empty when the diff touches no item"`
}

// AuditAnswerFinding is one checklist item a change touches.
type AuditAnswerFinding struct {
	Item int    `json:"item" desc:"the checklist item's number, 1 to 6, by this fixed checklist:\n1. secrets: keys, passwords, bearer tokens, API tokens, private keys, connection strings with credentials, in code, configuration or tests (a credential-shaped literal in a test counts: the human decides whether it is a fixture).\n2. network egress: new hosts or URLs the code reaches, NetworkPolicies, egress allowlists, proxies.\n3. RBAC and ServiceAccounts: Roles, ClusterRoles, bindings, ServiceAccounts, service account tokens.\n4. policy and workflow files: by path, the factory's policy, workflow and project files (factory/**), CI workflows (.forgejo/**, .gitea/**, .github/**), CODEOWNERS, the agents' instruction and harness files (AGENTS.md, CLAUDE.md, opencode.json). Source code is not this item, even code that names policy, workflow or risk types, functions or constants: the model review judges code.\n5. migrations: only those that drop or widen (DROP TABLE or COLUMN, a widened or removed CHECK, removed NOT NULL, UNIQUE or foreign key constraints, widened grants).\n6. sandbox contract: the worker image, pod specs, securityContext (privileged, capabilities, runAsNonRoot, readOnlyRootFilesystem), host paths and mounts, the agent harness configuration."`
	File string `json:"file,omitempty" max:"maxAuditField" desc:"the path; never quote a secret's value"`
	Line int    `json:"line,omitempty" desc:"the line in the new file, 0 for none"`
	Why  string `json:"why" max:"maxAuditWhy"`
}

// GateDecisionAnswer is the gate supervisor's decision.
type GateDecisionAnswer struct {
	Action         string `json:"action" enum:"wait,retry,cancel,escalate" desc:"wait: the activity makes progress and needs more time (checks queued or running, a pod pulling its image); its bound is extended once by its own length. retry: the attempt is stuck and a fresh attempt is likely to succeed (a hung poll, a lost connection); the attempt is stopped and runs again. cancel: it cannot succeed and no human could change that; the task fails with your diagnosis as the reason. escalate: a human must decide or act (access, infrastructure, a judgement); the task waits for a human with your diagnosis and recommendation."`
	Diagnosis      string `json:"diagnosis" max:"maxGateText" desc:"what is happening and why"`
	Recommendation string `json:"recommendation,omitempty" max:"maxGateText" desc:"what a human should do; required for escalate"`
	Answer         string `json:"answer,omitempty" desc:"the key of the one of human_answers you recommend"`
}

// DeltaAnswer is the delta planner's answer: one card.
type DeltaAnswer struct {
	Card DeltaCard `json:"card"`
}

// DeltaCard is the delta card as the planner writes it.
type DeltaCard struct {
	Title       string     `json:"title"`
	Why         string     `json:"why"`
	Summary     string     `json:"summary"`
	Brief       string     `json:"brief"`
	Directories []string   `json:"directories" desc:"the directories the card may change"`
	DependsOn   []string   `json:"depends_on" desc:"keys of the plan's existing cards"`
	Acceptance  []PlanItem `json:"acceptance" desc:"every criterion the card closes"`
	Size        string     `json:"size" enum:"small,medium,large"`
	Rung        string     `json:"rung,omitempty" enum:"cheap,frontier"`
	RungReason  string     `json:"rung_reason,omitempty"`
	Image       string     `json:"image,omitempty" desc:"the project's pinned image the card runs on"`
}

// HoldoutAnswer is the hold-out proposer's answer.
type HoldoutAnswer struct {
	Holdouts []holdoutProposal `json:"holdouts" max:"maxHoldouts" desc:"none when no claim is worth a test"`
}

// JudgementAnswer is a judgement criterion's verdict.
type JudgementAnswer struct {
	Verdict string `json:"verdict" enum:"met,unmet"`
	Reason  string `json:"reason" max:"maxReviewFinding" desc:"one sentence on what the artifact shows"`
}

// ScreenVerdict is the screening's verdict on an answer.
type ScreenVerdict struct {
	Verdict string   `json:"verdict" enum:"clear,suspect" desc:"clear: genuine, and safe for an agent to act on. suspect: anything else"`
	Reasons []string `json:"reasons" max:"maxScreenReasons" item:"maxScreenReason" desc:"one for each reason, the text it rests on quoted"`
}

// BriefAnswer is the brief writer's brief and detail.
type BriefAnswer struct {
	Brief  string `json:"brief" max:"briefChars" desc:"one to three sentences: what is being decided and why it needs a human; lead with what makes this card its own, not the boilerplate"`
	Detail string `json:"detail" max:"detailChars" desc:"the same facts in plain language: the numbers (costs, lease, revisions), the recommendation and the reasons a human decides, one line per fact, \"- \" before each; names the card itself shows (task, project, pull request, filed at) are not detail; a log's raw lines are never detail: state what a line found, not the line"`
}

// CaseBriefAnswer is the case writer's two story lines.
type CaseBriefAnswer struct {
	Happened string `json:"happened" max:"writerHappenedMax" desc:"one sentence: what stopped the card, in plain words; the card puts the money spent and the sessions before it: give no amount"`
	Left     string `json:"left" max:"writerLeftMax" desc:"one sentence: what is left of the card's work now, in words; the card puts the open findings and what a session costs after it"`
}

// NameAnswer is the plan namer's name.
type NameAnswer struct {
	Name string `json:"name" max:"planNameMax" desc:"short plain words that say what the plan does, fixes or enables, one line, no trailing period"`
}

// LivenessAnswer is the small class's answer to a rollout's liveness ask.
type LivenessAnswer struct {
	OK bool `json:"ok" desc:"true"`
}
