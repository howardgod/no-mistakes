package citest

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
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

func ptr(s string) *string { return &s }
