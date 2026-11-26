package main

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// requiredWorkflowTestHeadSHA is the commit the generated pipeline summary
// attestation binds to. Tests that execute the action pass the same value as
// the PR head SHA unless they are asserting a mismatch.
const requiredWorkflowTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

func pipelineSummaryWithStatuses(t *testing.T, review, testStep, document types.StepStatus) string {
	t.Helper()
	stepResults := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: review},
		{ID: "test", StepName: types.StepTest, Status: testStep},
		{ID: "document", StepName: types.StepDocument, Status: document},
		{ID: "pr", StepName: types.StepPR, Status: types.StepStatusRunning},
		{ID: "ci", StepName: types.StepCI, Status: types.StepStatusPending},
	}
	rounds := make(map[string][]*db.StepRound, len(stepResults))
	for _, sr := range stepResults {
		rounds[sr.ID] = []*db.StepRound{{Round: 1, Trigger: "initial", DurationMS: 1}}
	}
	md, _ := steps.BuildPipelineSummary(stepResults, rounds, requiredWorkflowTestHeadSHA)
	if md == "" {
		t.Fatal("BuildPipelineSummary returned empty markdown")
	}
	return md
}
