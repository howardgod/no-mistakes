package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestPRSignature_Publication(t *testing.T) {
	t.Parallel()
	const linked = "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
	const plain = "Updates from git push no-mistakes"
	for _, templated := range []bool{false, true} {
		for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
			t.Run(fmt.Sprintf("template=%v/%s", templated, mode), func(t *testing.T) {
				t.Parallel()
				sctx, ag, _ := templateTestContext(t)
				template := sctx.Config.PR.Template
				if !templated {
					template = ""
					ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
						data, _ := json.Marshal(prContent{Title: "feat(pipeline): choose signature", Body: "## What Changed\n\nKeep the signature configurable."})
						return &agent.Result{Output: data}, nil
					}
				}
				var defaultBody, defaultEvidence string
				for _, signature := range []string{"", config.PRSignatureLink, config.PRSignaturePlain} {
					trusted, err := config.LoadRepoFromBytes([]byte(fmt.Sprintf("pr:\n  template: %q\n  appendix: %s\n  signature: %q\n", template, mode, signature)))
					if err != nil {
						t.Fatal(err)
					}
					pushed := &config.RepoConfig{PR: config.PRRaw{Signature: config.PRSignaturePlain}}
					sctx.Config.PR = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(pushed, trusted, true)).PR
					env, _ := fakeGH(t, "")
					bodyFile := filepath.Join(t.TempDir(), "body.md")
					sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
					out, err := (&PRStep{}).Execute(sctx)
					if err != nil || out == nil || out.PRURL == "" {
						t.Fatalf("signature %q publication: %+v, %v", signature, out, err)
					}
					raw, err := os.ReadFile(bodyFile)
					if err != nil {
						t.Fatal(err)
					}
					body := string(raw)
					evidence := body
					if templated {
						parts, err := parsePROwnedBody(body)
						if err != nil || !parts.managed || !strings.HasPrefix(parts.before, filledPRTemplate) {
							t.Fatalf("signature %q changed author narrative or ownership: %+v, %v", signature, parts, err)
						}
						evidence = parts.appendix
					}
					t.Logf("signature %q published body:\n%s", signature, body)
					want := linked
					if signature == config.PRSignaturePlain {
						want = plain
						if strings.Contains(body, "https://github.com/kunchenguid/no-mistakes") {
							t.Fatalf("plain publication retained the project link:\n%s", body)
						}
					}
					if strings.Count("\n"+evidence+"\n", "\n"+want+"\n") != 1 {
						t.Fatalf("signature %q missing or duplicated:\n%s", signature, evidence)
					}
					if strings.Count(body, pipelineAttestationCommentPrefix) != 1 || parsePipelineAttestationForTest(t, body).HeadSHA != sctx.Run.HeadSHA {
						t.Fatalf("signature %q changed the head-bound attestation:\n%s", signature, body)
					}
					switch signature {
					case "":
						defaultBody, defaultEvidence = body, evidence
					case config.PRSignatureLink:
						if body != defaultBody {
							t.Fatal("explicit link differs byte-for-byte from the default")
						}
					case config.PRSignaturePlain:
						if evidence != strings.Replace(defaultEvidence, linked, plain, 1) {
							t.Fatal("plain mode changed more than the signature in the evidence")
						}
					}
				}
			})
		}
	}
}
