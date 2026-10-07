package citest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A run whose trusted config came from an operator-selected branch certifies
// only a PR that targets that branch. The forge is the fake gh CLI; the live
// base it reports stands in for a retarget made on the forge mid-run.
func TestCIStep_PinnedTrustedSourceRequiresTheLivePRBase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		live    *string
		wantErr string
	}{
		{name: "retargeted_away", live: ptr("main"), wantErr: `live PR base "main" differs from pinned trusted config branch "verify"`},
		{name: "unreadable_base", live: nil, wantErr: `read live PR base for trusted config branch "verify"`},
		{name: "still_on_pinned_branch", live: ptr("verify")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			env := stepstest.FakeCIGH(t, "MERGED", "[]")
			if tc.live != nil {
				env = append(env, "FAKE_CLI_PR_BASE="+*tc.live)
			}
			prURL := "https://github.com/test/repo/pull/42"
			sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Run.PRURL = &prURL
			sctx.Run.PRBaseBranch = ptr("verify")
			sctx.Run.TrustedConfigBranch = ptr("verify")

			outcome, err := (&steps.CIStep{}).Execute(sctx)
			if tc.wantErr == "" {
				if err != nil || outcome == nil || outcome.NeedsApproval {
					t.Fatalf("outcome = %#v, err = %v; want the merged PR on its pinned base to exit cleanly", outcome, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// The retry of a CI repair a protected-path refusal interrupted parks its own
// failures for a decision; a retargeted PR must still fail the step before
// that repair runs, never become an approvable park.
func TestCIStep_PinnedTrustedSourceRetargetFailsARefusalRetry(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	prURL := "https://github.com/test/repo/pull/42"
	sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(stepstest.FakeCIGH(t, "OPEN", "[]"), "FAKE_CLI_PR_BASE=main")
	sctx.Run.PRURL = &prURL
	sctx.Run.PRBaseBranch = ptr("verify")
	sctx.Run.TrustedConfigBranch = ptr("verify")
	sctx.Fixing = true
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(stepResult.ID, `{"findings":[{"id":"protected-path-refusal","severity":"error","description":"refused","action":"ask-user"}],"summary":"refused"}`); err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	want := `live PR base "main" differs from pinned trusted config branch "verify"`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("outcome = %#v, err = %v; want the step to fail with %q", outcome, err, want)
	}
}

// A failed live-base read during polling is one unverified poll, like every
// other monitor read: it neither fails the run nor lets that poll mark the PR
// ready or exit merged, and a read that keeps failing parks for a decision
// instead of spinning. The fake gh answers the base reads in order, the first
// being the step's own pre-monitor read, and repeats the last answer.
func TestCIStep_PinnedTrustedSourceToleratesATransientBaseReadWhilePolling(t *testing.T) {
	const passing = `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	for _, tc := range []struct {
		name  string
		state string
		bases []string
		// cancelAfter cancels the step at that wait; 0 lets it run to its own exit.
		cancelAfter int
		wantPark    bool
		wantWaits   int
		wantReady   []bool
	}{
		{
			name:      "merged_only_after_the_base_is_verified_again",
			state:     "MERGED",
			bases:     []string{"verify", "!HTTP 502: rate limited", "verify"},
			wantWaits: 1,
			wantReady: []bool{false},
		},
		{
			name:        "ready_signal_cleared_by_the_unverified_poll",
			state:       "OPEN",
			bases:       []string{"verify", "verify", "!HTTP 502: rate limited", "verify"},
			cancelAfter: 2,
			wantWaits:   2,
			wantReady:   []bool{true, false},
		},
		{
			name:      "every_polled_read_failing_parks_for_a_decision",
			state:     "OPEN",
			bases:     []string{"verify", "!HTTP 401: bad credentials"},
			wantPark:  true,
			wantWaits: 5,
			wantReady: []bool{false, false, false, false, false, false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			basesPath := filepath.Join(t.TempDir(), "bases.txt")
			if err := os.WriteFile(basesPath, []byte(strings.Join(tc.bases, "\n")), 0o644); err != nil {
				t.Fatal(err)
			}
			env := append(stepstest.FakeCIGH(t, tc.state, passing), "FAKE_CLI_PR_BASE_SEQ_PATH="+basesPath)
			prURL := "https://github.com/test/repo/pull/42"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Ctx = ctx
			sctx.Env = env
			sctx.Run.PRURL = &prURL
			sctx.Run.PRBaseBranch = ptr("verify")
			sctx.Run.TrustedConfigBranch = ptr("verify")
			var ready []bool
			sctx.CIReadinessChanged = func(r, _ bool) { ready = append(ready, r) }

			waits := 0
			step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
				waits++
				if waits == tc.cancelAfter {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			outcome, err := step.Execute(sctx)
			if tc.wantPark {
				if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, `live PR base for trusted config branch \"verify\"`) {
					t.Fatalf("outcome = %#v, err = %v; want a park naming the unreadable live PR base", outcome, err)
				}
			} else if tc.cancelAfter > 0 {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want the monitor still polling after the failed base read", err)
				}
			} else if err != nil || outcome == nil || outcome.NeedsApproval {
				t.Fatalf("outcome = %#v, err = %v; want a clean merged exit", outcome, err)
			}
			if waits != tc.wantWaits {
				t.Fatalf("waits = %d, want %d", waits, tc.wantWaits)
			}
			if !slices.Equal(ready, tc.wantReady) {
				t.Fatalf("readiness changes = %v, want %v", ready, tc.wantReady)
			}
		})
	}
}

func ptr(s string) *string { return &s }
