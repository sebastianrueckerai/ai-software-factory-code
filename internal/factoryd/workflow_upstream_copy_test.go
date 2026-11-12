package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"
)

const lostLine = "docker.io/library/golang:1.26.11-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (h *workflowHarness) onCopy(findings []string, err error) *[]string {
	var heads []string
	h.env.OnActivity(ActivityCopyUpstreamImages, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) ([]string, error) {
			heads = append(heads, sha)
			return findings, err
		})
	return &heads
}

// A branch's image lines are copied before its pull request opens; a line
// the upstream cannot serve is the card's finding and the run goes on.
func TestWorkflowCopiesUpstreamImagesBeforeOpeningThePullRequest(t *testing.T) {
	h := newWorkflowHarness(t)
	order := h.activityOrder()
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s1", "c1")})
	finding := lostLine + ": the upstream does not serve this digest"
	copied := h.onCopy([]string{finding}, nil)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if !slices.Equal(*copied, []string{"c1"}) || !before(*order, ActivityCopyUpstreamImages, ActivityOpenPullRequest) {
		t.Errorf("copies on %v, order %v; want one copy of c1 before OpenPullRequest", *copied, *order)
	}
	if !slices.Contains(steps, "pr c1 after 0 cards") {
		t.Errorf("steps = %v, want the pull request opened", steps)
	}
	var got []string
	for _, r := range h.recorded {
		if r.step == "stage_findings" && r.detail["stage"] == "upstream-copy" && r.detail["to"] == "card" {
			got = append(got, fmt.Sprint(r.detail["findings"]))
		}
	}
	if want := []string{fmt.Sprint([]string{finding})}; !slices.Equal(got, want) {
		t.Errorf("card findings = %v, want %v", got, want)
	}
}

// With no finding nothing is recorded.
func TestWorkflowRecordsNoFindingForACleanCopy(t *testing.T) {
	h := newWorkflowHarness(t)
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s1", "c1")})
	h.onCopy(nil, nil)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	for _, r := range h.recorded {
		if r.detail["stage"] == "upstream-copy" {
			t.Errorf("recorded %v for a clean copy", r)
		}
	}
}

// A factory pull request already open is copied for before its stages wait.
func TestWorkflowCopiesUpstreamImagesBeforeAFactoryPullRequestsStages(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "queued", FactoryPR: true, PR: PullRequest{Number: 9, URL: "https://git.example/sf/repo/pulls/9"}, Head: "c7"}
	order := h.activityOrder()
	h.onChecks(ChecksResult{State: ChecksSuccess})
	copied := h.onCopy(nil, nil)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if !slices.Equal(*copied, []string{"c7"}) || !before(*order, ActivityCopyUpstreamImages, ActivityWaitChecks) || slices.Contains(*order, ActivityOpenPullRequest) {
		t.Errorf("copies on %v, order %v; want c7 copied before the checks wait and no pull request opened", *copied, *order)
	}
}

// A failed copy fails the attempt for the retry machinery.
func TestWorkflowFailsTheAttemptWhenTheCopyErrors(t *testing.T) {
	h := newWorkflowHarness(t)
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s1", "c1")})
	h.onCopy(nil, fmt.Errorf("store refused"))
	h.run(t)
	if len(h.finals) != 1 || h.finals[0][0] != "failed" || !strings.Contains(h.finals[0][1], "copy upstream images:") {
		t.Fatalf("finals = %v, want failed with the copy's reason", h.finals)
	}
	if slices.Contains(steps, "pr c1 after 0 cards") {
		t.Errorf("the pull request opened after a failed copy: %v", steps)
	}
}

// A run from before the change id copies nothing.
func TestWorkflowBeforeUpstreamCopyOpensWithoutCopying(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("upstream-copy", workflow.DefaultVersion, versionUpstreamCopy).Return(workflow.DefaultVersion)
	order := h.activityOrder()
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s1", "c1")})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if slices.Contains(*order, ActivityCopyUpstreamImages) || !slices.Contains(*order, ActivityOpenPullRequest) {
		t.Errorf("order %v: want OpenPullRequest and no copy", *order)
	}
}

// A head the agent moves past its gates is copied for before its checks
// wait, as a verified head is, at the policy gate and at the merge.
func TestWorkflowCopiesUpstreamImagesOfAMovedHead(t *testing.T) {
	for name, setup := range map[string]func(g *gateHarness){
		"at the gates": func(g *gateHarness) {
			g.policyErrs = []error{headMovedError("pull request #9's head moved to c2 after its checks passed on c1")}
		},
		"at the merge": func(g *gateHarness) {
			g.merge = []MergeResult{{MovedHead: true, Refusal: "pull request #9's head moved to c2 after its gates ran on c1"}, {Merged: true}}
			g.signalAfter(4*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGateHarness(t)
			g.onSessions(pushed("s1", "c1"))
			g.branchHead = "c2"
			setup(g)
			g.onChecks(ChecksResult{State: ChecksSuccess})
			copied := g.onCopy(nil, nil)
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			if final := g.run(t); final != "done" {
				t.Fatalf("final = %q, finals = %v", final, g.finals)
			}
			if !slices.Equal(*copied, []string{"c1", "c2"}) {
				t.Errorf("copies on %v, want c1 then c2 once each", *copied)
			}
		})
	}
}
