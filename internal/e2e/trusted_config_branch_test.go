//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	trustedSourceRemote = "https://github.com/acme/trusted-source-e2e.git"
	trustedSourceBranch = "feature/source"
	trustedSourceOptIn  = "repository_overrides:\n  " + trustedSourceRemote + ":\n    trusted_config_branches: [verify]\n"
)

// These journeys cross the real CLI, the gate, the daemon, local Git fetches
// and the pipeline. GitHub and the agent are fake processes that capture the
// PR arguments and every agent prompt; no real forge or SynArp is involved.
//
// main, verify and feature/source each carry a different gate marker, review
// rule and PR template. feature/source also tries to own the trust root.
func TestTrustedConfigBranchJourney(t *testing.T) {
	for _, tc := range []struct {
		name string
		fx   trustedSourceFixture
		flag bool
		// want is the branch whose gate, rule and template must be the only
		// ones used; "" means none of them (defaults).
		want   string
		wantPR string
	}{
		{name: "authorized_explicit_base", fx: trustedSourceFixture{optIn: true}, flag: true, want: "verify", wantPR: "verify"},
		{name: "unauthorized_explicit_base", fx: trustedSourceFixture{}, flag: true, want: "main", wantPR: "verify"},
		{name: "authorized_without_explicit_base", fx: trustedSourceFixture{optIn: true}, want: "main", wantPR: "main"},
		// A readable verify tree without .no-mistakes.yaml is valid and uses
		// defaults; it never borrows main's policy.
		{name: "authorized_source_without_config_uses_defaults", fx: trustedSourceFixture{optIn: true, verifyWithoutConfig: true}, flag: true, want: "", wantPR: "verify"},
		// With allow_repo_commands on main, the pushed pr.base_branch may pick the
		// PR target, as today, but it never picks the trusted config source.
		{name: "pushed_base_branch_cannot_select_the_source", fx: trustedSourceFixture{optIn: true, mainAllowRepoCommands: true, featurePRBase: "verify"}, want: "main", wantPR: "verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ghLog := setupTrustedSourceJourney(t, tc.fx, trustedSourceScenario(t, false))
			startTrustedSourceRun(t, h, tc.flag)
			run := h.WaitForRun(trustedSourceBranch, 180*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run did not complete: %s %v", run.Status, deref(run.Error))
			}
			assertTrustedSourceUsed(t, h, run, ghLog, tc.want, tc.want, tc.wantPR)
			if tc.want == "verify" && !strings.Contains(reviewPrompt(t, h), `trusted config from branch "verify" at `) {
				t.Fatal("review prompt did not name the alternate source branch and SHA")
			}
		})
	}

	// Startup reads the source before any agent runs: an unparseable verify
	// config stops the run instead of falling back to main or the pushed branch.
	t.Run("unparseable_source_fails_before_any_agent", func(t *testing.T) {
		h, _ := setupTrustedSourceJourney(t, trustedSourceFixture{optIn: true, verifyConfig: "gates: [unterminated\n"}, trustedSourceScenario(t, false))
		startTrustedSourceRun(t, h, true)
		run := h.WaitForRun(trustedSourceBranch, 60*time.Second)
		if run.Status != types.RunFailed {
			t.Fatalf("run status = %s, want failed", run.Status)
		}
		if msg := deref(run.Error); !strings.Contains(msg, `trusted source branch "verify"`) || !strings.Contains(msg, "unparseable") {
			t.Fatalf("run error = %q, want it to name verify's unparseable config", msg)
		}
		if invs := h.AgentInvocations(); len(invs) != 0 {
			t.Fatalf("agent ran %d turns after the trusted source could not be read", len(invs))
		}
		t.Logf("EVIDENCE failed run error: %s", deref(run.Error))
	})

	// A daemon crash while the run is parked: recovery re-reads policy from the
	// same live verify branch (the template moved on), keeps the pinned gate
	// list (verify's gate was renamed) and never touches main.
	t.Run("recovery_keeps_the_pinned_source", func(t *testing.T) {
		h, ghLog := setupTrustedSourceJourney(t, trustedSourceFixture{optIn: true}, trustedSourceScenario(t, true))
		startTrustedSourceRun(t, h, true)
		parked := waitForStepStatus(t, h, trustedSourceBranch, types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

		h.CommitChange("verify", ".no-mistakes.yaml", trustedSourceFixtureConfig("verify2", ""), "rename verify gate")
		h.CommitChange("verify", "docs/pr.md", "# VERIFY2 TEMPLATE", "move verify template")
		pushTrustedSourceBranch(t, h, "verify")
		crashAndRestartDaemon(t, h)
		respondAfterRestart(t, h, parked.ID, types.ActionApprove)

		run := h.WaitForRun(trustedSourceBranch, 180*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("recovered run did not complete: %s %v", run.Status, deref(run.Error))
		}
		// The pinned gate list survives; the template is re-read from live verify.
		assertTrustedSourceUsed(t, h, run, ghLog, "verify", "verify2", "verify")
	})

	t.Run("recovery_refuses_a_revoked_source", func(t *testing.T) {
		h, ghLog := setupTrustedSourceJourney(t, trustedSourceFixture{optIn: true}, trustedSourceScenario(t, true))
		startTrustedSourceRun(t, h, true)
		waitForStepStatus(t, h, trustedSourceBranch, types.StepReview, types.StepStatusAwaitingApproval, 120*time.Second)

		h.globalConfigExtra = ""
		h.writeGlobalConfig()
		crashAndRestartDaemon(t, h)

		run := h.WaitForRun(trustedSourceBranch, 60*time.Second)
		if run.Status != types.RunFailed {
			t.Fatalf("run status after revocation = %s, want failed", run.Status)
		}
		for _, marker := range []string{"main", "verify", "feature"} {
			if gateRan(run, marker) {
				t.Fatalf("gate %s ran after the source was revoked", marker)
			}
		}
		if _, err := os.Stat(ghLog); err == nil {
			for _, inv := range readGHStubInvocations(t, ghLog) {
				if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
					t.Fatalf("a PR was created after the source was revoked: %+v", inv)
				}
			}
		}
		// The run record carries recovery's generic crash error; the specific
		// refusal is in the daemon log.
		logData, err := os.ReadFile(paths.WithRoot(h.NMHome).DaemonLog())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(logData), `trusted config branch \"verify\" for run `+run.ID+` is no longer authorized`) {
			t.Fatalf("daemon log does not record why recovery refused run %s", run.ID)
		}
		t.Logf("EVIDENCE revoked recovery: status=%s error=%q (refusal reason found in daemon log)", run.Status, deref(run.Error))
	})
}

type trustedSourceFixture struct {
	optIn                 bool
	mainAllowRepoCommands bool
	verifyWithoutConfig   bool
	verifyConfig          string // overrides verify's generated config when set
	featurePRBase         string
}

func setupTrustedSourceJourney(t *testing.T, fx trustedSourceFixture, scenario string) (*Harness, string) {
	t.Helper()
	override := ""
	if fx.optIn {
		override = trustedSourceOptIn
	}
	optOut := false
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, AllowRepoCommands: &optOut, GlobalConfigExtra: override})
	configureGitURLRewrite(t, h, trustedSourceRemote, h.UpstreamDir)
	if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "set-url", "origin", trustedSourceRemote); err != nil {
		t.Fatalf("set origin: %v\n%s", err, out)
	}
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-trusted-source.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "acme/trusted-source-e2e")

	mainConfig := trustedSourceFixtureConfig("main", "")
	if fx.mainAllowRepoCommands {
		mainConfig = strings.Replace(mainConfig, "allow_repo_commands: false", "allow_repo_commands: true", 1)
	}
	h.CommitChange("main", ".no-mistakes.yaml", mainConfig, "configure main policy")
	h.CommitChange("main", "docs/pr.md", "# MAIN TEMPLATE", "main template")
	pushTrustedSourceBranch(t, h, "main")

	verifyConfig := trustedSourceFixtureConfig("verify", "")
	if fx.verifyConfig != "" {
		verifyConfig = fx.verifyConfig
	}
	h.CommitChange("verify", "docs/pr.md", "# VERIFY TEMPLATE", "verify template")
	if fx.verifyWithoutConfig {
		if out, err := h.runGit(context.Background(), h.WorkDir, "rm", "-q", ".no-mistakes.yaml"); err != nil {
			t.Fatalf("remove verify config: %v\n%s", err, out)
		}
		if out, err := h.runGit(context.Background(), h.WorkDir, "commit", "-q", "-m", "verify has no policy"); err != nil {
			t.Fatalf("commit verify without config: %v\n%s", err, out)
		}
	} else {
		h.CommitChange("verify", ".no-mistakes.yaml", verifyConfig, "configure verify policy")
	}
	pushTrustedSourceBranch(t, h, "verify")

	if out, err := h.runGit(context.Background(), h.WorkDir, "checkout", "-b", trustedSourceBranch, "verify"); err != nil {
		t.Fatalf("create feature: %v\n%s", err, out)
	}
	h.CommitChange(trustedSourceBranch, "feature.txt", "feature\n", "add feature")
	// The pushed branch tries to own the trust root and the gate. It cannot.
	featureConfig := trustedSourceFixtureConfig("feature", fx.featurePRBase) + "trusted_config_branches: [feature]\n"
	h.CommitChange(trustedSourceBranch, ".no-mistakes.yaml", featureConfig, "attempt feature policy")
	h.CommitChange(trustedSourceBranch, "docs/pr.md", "# FEATURE TEMPLATE", "feature template")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return h, ghLog
}

func startTrustedSourceRun(t *testing.T, h *Harness, explicitBase bool) {
	t.Helper()
	args := []string{"axi", "run", "--intent", "validate feature policy"}
	if explicitBase {
		args = append(args, "--base-branch", "verify")
	}
	if out, err := h.Run(args...); err != nil {
		t.Logf("axi run returned before a terminal state: %v\n%s", err, out)
	}
}

func pushTrustedSourceBranch(t *testing.T, h *Harness, branch string) {
	t.Helper()
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", branch); err != nil {
		t.Fatalf("push %s: %v\n%s", branch, err, out)
	}
}

// assertTrustedSourceUsed checks that only gateMarker's gate ran, only
// sourceMarker's review rule reached the reviewer, only sourceMarker's template
// shaped the PR, and the PR targets wantPR. An empty marker means defaults.
func assertTrustedSourceUsed(t *testing.T, h *Harness, run *ipc.RunInfo, ghLog, gateMarker, sourceMarker, wantPR string) {
	t.Helper()
	for _, marker := range []string{"main", "verify", "verify2", "feature"} {
		if ran := gateRan(run, marker); ran != (marker == gateMarker) {
			t.Fatalf("gate.lint.%s ran=%v, want only %q's gate; steps=%+v", marker, ran, gateMarker, run.Steps)
		}
	}
	prompt := reviewPrompt(t, h)
	for _, marker := range []string{"main", "verify", "feature"} {
		if has := strings.Contains(prompt, strings.ToUpper(marker)+" REVIEW RULE"); has != (marker == sourceMarker || (marker == "verify" && sourceMarker == "verify2")) {
			t.Fatalf("review prompt has %s rule=%v, want only %q's rule", marker, has, sourceMarker)
		}
	}
	var created *ghStubInvocation
	for _, inv := range readGHStubInvocations(t, ghLog) {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			inv := inv
			created = &inv
		}
	}
	if created == nil {
		t.Fatal("PR creation missing")
	}
	if created.Base != wantPR {
		t.Fatalf("PR base = %q, want %q", created.Base, wantPR)
	}
	for _, marker := range []string{"main", "verify", "verify2", "feature"} {
		template := "# " + strings.ToUpper(marker) + " TEMPLATE"
		if has := strings.Contains(created.Body, template+"\n"); has != (marker == sourceMarker) {
			t.Fatalf("PR body has %q=%v, want only %q's template:\n%s", template, has, sourceMarker, created.Body)
		}
	}
	t.Logf("EVIDENCE run %s: gate=%q rule/template=%q PR base=%q", run.ID, gateMarker, sourceMarker, created.Base)
}

// gateRan reports whether marker's gate executed. A pinned gate list records
// every gate step up front as pending, so presence alone proves nothing.
func gateRan(run *ipc.RunInfo, marker string) bool {
	step, ok := findStep(run.Steps, types.StepName("gate.lint."+marker))
	return ok && step.Status != types.StepStatusPending && step.Status != types.StepStatusSkipped
}

// trustedSourceScenario fills each pinned template with its own marker. With
// park, the first review raises one warning so the run waits for a human.
func trustedSourceScenario(t *testing.T, park bool) string {
	t.Helper()
	fallback, err := os.ReadFile(cleanReviewScenario(t))
	if err != nil {
		t.Fatal(err)
	}
	content := "actions:\n"
	for _, marker := range []string{"MAIN", "VERIFY", "VERIFY2", "FEATURE"} {
		content += fmt.Sprintf("  - match: %q\n    structured:\n      title: %q\n      body: %q\n", "Trusted repository template (JSON string):\n\"# "+marker+" TEMPLATE\"", "feat: source policy", "# "+marker+" TEMPLATE\nFilled from pinned source")
	}
	if park {
		content += `  - match: "` + reviewTurnMarker + `"
    structured:
      findings:
        - id: "source-check"
          severity: warning
          description: "needs a human look before the daemon restarts"
          file: "feature.txt"
          action: ask-user
          review_scope: source
      summary: "one question"
      risk_level: low
      risk_rationale: "parks the run"
      risk_scope: source-or-external
`
	}
	content += strings.TrimPrefix(string(fallback), "actions:\n")
	path := filepath.Join(t.TempDir(), "trusted-source-scenario.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func trustedSourceFixtureConfig(marker, prBase string) string {
	pr := "pr:\n  template: docs/pr.md\n"
	if prBase != "" {
		pr += "  base_branch: " + prBase + "\n"
	}
	return fmt.Sprintf("allow_repo_commands: false\nreview:\n  path_instructions:\n    - path: '*.txt'\n      instructions: '%s REVIEW RULE'\n%sgates:\n  - name: %s\n    after: lint\n    command: 'echo %s-gate'\n", strings.ToUpper(marker), pr, marker, marker)
}

// crashAndRestartDaemon kills the daemon without a graceful stop, which is the
// only way to exercise parked-run recovery, then starts a new one.
func crashAndRestartDaemon(t *testing.T, h *Harness) {
	t.Helper()
	pid, err := daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil || pid <= 0 {
		t.Fatalf("read daemon pid: %v (pid=%d)", err, pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill daemon %d: %v", pid, err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("daemon start after crash: %v\n%s", err, out)
	}
}

func respondAfterRestart(t *testing.T, h *Harness, runID string, action types.ApprovalAction) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		if err = h.respondError(runID, types.StepReview, action, nil); err == nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("respond after restart: %v", err)
}
