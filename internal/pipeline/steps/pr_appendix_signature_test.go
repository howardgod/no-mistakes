package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The two lines an appendix carried before the signature was removed. Tests
// seed them into existing PR bodies; nothing writes them any more.
const (
	legacyLinkedPRSignature = "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
	legacyPlainPRSignature  = "Updates from git push no-mistakes"
	testRiskLine            = "✅ Low: test-only change"
	legacyAttestation       = `<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","steps":[]} -->`
)

// The three ways a run fills the PR narrative.
const (
	narrativeGenerated    = "generated"
	narrativeRepoTemplate = "pr.template"
	narrativeUserTemplate = "pr.user_template"
)

// signatureTestContext is templateTestContext set up for one narrative source
// and appendix mode, with a recorded low risk level. It returns the narrative
// the drafting agent answers with.
func signatureTestContext(t *testing.T, narrative, mode string) (*pipeline.StepContext, string) {
	t.Helper()
	sctx, ag, _ := templateTestContext(t)
	sctx.Config.PR.Appendix = mode
	filled := filledPRTemplate
	switch narrative {
	case narrativeGenerated:
		sctx.Config.PR.Template = ""
		filled = "## What Changed\n\nDrop the signature line."
	case narrativeUserTemplate:
		userTemplateHome(t, map[string]string{"causal.md": testUserPRTemplate})
		sctx.Config.PR.UserTemplate = "causal.md"
		filled = filledUserPRTemplate
	}
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat(pipeline): drop the signature", Body: filled})
		return &agent.Result{Output: data}, nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview {
			if err := sctx.DB.SetStepFindings(step.ID, findingsJSON(t, types.Findings{RiskLevel: "low", RiskRationale: "test-only change"})); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sctx, filled
}

func assertNoPRSignature(t *testing.T, label, body string) {
	t.Helper()
	if strings.Contains(body, "git push no-mistakes") {
		t.Fatalf("%s: body carries a signature line:\n%s", label, body)
	}
}

func attestationMarkerForTest(t *testing.T, body string) string {
	t.Helper()
	marker := extractPipelineAttestationMarker(body)
	if marker == "" || strings.Count(body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("want exactly one attestation:\n%s", body)
	}
	return marker
}

// No narrative source and no appendix mode writes a signature. The minimal
// tail is exactly the risk line and the attestation.
func TestPRAppendix_WritesNoSignature(t *testing.T) {
	for _, narrative := range []string{narrativeGenerated, narrativeRepoTemplate, narrativeUserTemplate} {
		for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
			t.Run(narrative+"/"+mode, func(t *testing.T) {
				sctx, filled := signatureTestContext(t, narrative, mode)
				content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
				if err != nil {
					t.Fatal(err)
				}
				body := content.Body
				assertNoPRSignature(t, narrative+"/"+mode, body)
				marker := attestationMarkerForTest(t, body)
				if parsePipelineAttestationForTest(t, body).HeadSHA != sctx.Run.HeadSHA {
					t.Fatalf("attestation not bound to the run head:\n%s", body)
				}
				tail := body
				if narrative != narrativeGenerated {
					parts, err := parsePROwnedBody(body)
					if err != nil || !parts.managed || !strings.HasPrefix(parts.before, filled) {
						t.Fatalf("templated body not owned: %+v, %v\n%s", parts, err, body)
					}
					tail = parts.appendix
				}
				if mode == config.PRAppendixMinimal {
					want := testRiskLine + "\n\n" + marker
					if narrative == narrativeGenerated {
						want = "\n\n" + want
					}
					if !strings.HasSuffix(tail, want) || (narrative != narrativeGenerated && tail != want) {
						t.Fatalf("minimal tail is not the risk line and the attestation:\n%s", tail)
					}
					return
				}
				if !strings.Contains(tail, "## Pipeline\n\n"+pipelineAttestationCommentPrefix) {
					t.Fatalf("Pipeline section does not open with the attestation:\n%s", tail)
				}
			})
		}
	}
}

// A PR published before the signature was removed carries a signed owned
// appendix. The next run must recognize it, keep the author text around it,
// and rewrite the appendix without the signature.
func TestPRAppendix_RewritesASignedAppendixUnsigned(t *testing.T) {
	const after = "\n\nAuthor note kept below the appendix.\n"
	for _, narrative := range []string{narrativeRepoTemplate, narrativeUserTemplate} {
		for _, mode := range []string{config.PRAppendixMinimal, config.PRAppendixFull} {
			for _, signature := range []string{legacyPlainPRSignature, legacyLinkedPRSignature} {
				t.Run(fmt.Sprintf("%s/%s/%s", narrative, mode, signature), func(t *testing.T) {
					sctx, filled := signatureTestContext(t, narrative, mode)
					signed := filled + "\n" + wrapPRAppendix("⚠️ Medium: old risk\n\n"+signature+"\n\n"+legacyAttestation) + after
					if _, err := parsePROwnedBody(signed); err != nil {
						t.Fatalf("seeded signed appendix is not owned: %v", err)
					}
					env, _ := fakeGH(t, "https://github.com/test/repo/pull/42")
					bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
					if err := os.WriteFile(bodyFile, []byte(signed), 0o644); err != nil {
						t.Fatal(err)
					}
					sctx.Env = env
					if _, err := (&PRStep{}).Execute(sctx); err != nil {
						t.Fatalf("update of a signed appendix failed: %v", err)
					}
					body := string(mustRead(t, bodyFile))
					parts, err := parsePROwnedBody(body)
					if err != nil {
						t.Fatalf("updated body is not owned: %v\n%s", err, body)
					}
					if strings.Count(body, prAppendixStart) != 1 || strings.Count(body, prAppendixEnd) != 1 {
						t.Fatalf("appendix duplicated:\n%s", body)
					}
					if parts.before != filled+"\n" || parts.after != after {
						t.Fatalf("author text changed: before=%q after=%q", parts.before, parts.after)
					}
					assertNoPRSignature(t, mode, body)
					marker := attestationMarkerForTest(t, body)
					if parsePipelineAttestationForTest(t, body).HeadSHA != sctx.Run.HeadSHA {
						t.Fatalf("attestation not rebound to the run head:\n%s", body)
					}
					if mode == config.PRAppendixMinimal && parts.appendix != testRiskLine+"\n\n"+marker {
						t.Fatalf("minimal appendix = %q", parts.appendix)
					}
				})
			}
		}
	}
}

// Restamping rewrites only the attestation, so a signed and an unsigned
// appendix both stay owned and keep their layout.
func TestRebindOwnedPRAttestation_KeepsEitherLayout(t *testing.T) {
	t.Parallel()
	const newHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	steps := []*db.StepResult{{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted}}
	for _, appendix := range []string{
		testRiskLine + "\n\n" + legacyAttestation,
		testRiskLine + "\n\n" + legacyPlainPRSignature + "\n\n" + legacyAttestation,
	} {
		body := "Narrative.\n" + wrapPRAppendix(appendix)
		updated, rebound, err := rebindOwnedPRAttestation(body, newHead, steps, pipelineAttestationPolicy{})
		if err != nil || !rebound {
			t.Fatalf("rebind %q: %v, %v", appendix, rebound, err)
		}
		parts, err := parsePROwnedBody(updated)
		if err != nil || parsePipelineAttestationForTest(t, updated).HeadSHA != newHead {
			t.Fatalf("rebound body not owned or not restamped: %v\n%s", err, updated)
		}
		if strings.Contains(appendix, legacyPlainPRSignature) != strings.Contains(parts.appendix, legacyPlainPRSignature) {
			t.Fatalf("rebind changed the appendix layout:\n%s", updated)
		}
	}
}

// The Pipeline header is the attestation alone, so body truncation still
// keeps it and no step is folded into the fixed header.
func TestSplitPipelineSectionHeader_NoSignature(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "lint", StepName: types.StepLint, Status: types.StepStatusCompleted},
	}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		md, _ := BuildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, provider)
		assertNoPRSignature(t, string(provider), md)
		header, updates := splitPipelineSectionHeader(md)
		want := "## Pipeline\n\n"
		if provider == scm.ProviderGitHub {
			want += buildPipelineAttestation(steps, nil, testPipelineHeadSHA) + "\n\n"
		}
		if header != want || header+updates != md {
			t.Fatalf("%s: header = %q, want %q", provider, header, want)
		}
	}
}
