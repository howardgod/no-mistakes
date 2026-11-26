//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The two signature lines are the documented contract in
// reference/repo-config.md (pr.signature); the linked one is also the
// require-no-mistakes action's SIGNATURE_MARKER.
const (
	linkedPRSignature = "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
	plainPRSignature  = "Updates from git push no-mistakes"
	signatureLinkURL  = "](https://github.com/kunchenguid/no-mistakes)"
)

// TestPRSignatureJourney drives pr.signature through the real binary, daemon,
// gate hook, and the stateful gh stub, reading each PR body back as published:
//
//   - no pr.signature writes the plain line with no link, in the full and
//     minimal appendix and in a pr.template appendix;
//   - pr.signature: link committed only on the pushed branch is ignored, even
//     with allow_repo_commands: true on the trusted branch;
//   - pr.signature: link on the trusted default branch writes the linked line;
//   - an unrecognized pr.signature on the trusted branch fails the run closed.
func TestPRSignatureJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	baseConfig := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\nno_ci: true\n"
	setTrustedConfig := func(extra, message string) {
		t.Helper()
		h.CommitChange("main", ".no-mistakes.yaml", baseConfig+extra, message)
		if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
			t.Fatalf("push main: %v\n%s", err, out)
		}
	}
	axiRun := func(branch string, pushedConfig string) (string, error) {
		t.Helper()
		h.CommitChange(branch, strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n", "add "+branch)
		if pushedConfig != "" {
			h.CommitChange(branch, ".no-mistakes.yaml", pushedConfig, "pushed-branch pr.signature")
		}
		return h.RunInDir(h.AddWorktree(branch), "axi", "run", "--intent", "exercise pr.signature", "--skip", "ci")
	}
	startRun := func(branch string, pushedConfig string) {
		t.Helper()
		if out, err := axiRun(branch, pushedConfig); err != nil {
			t.Fatalf("axi run %s: %v\n%s", branch, err, out)
		}
	}
	publishedBody := func(branch, evidenceName string) string {
		t.Helper()
		waitPublished(t, h, branch, "", branch)
		body := livePRBody(t, statePath, branch)
		saveSignatureEvidence(t, evidenceName, body)
		return body
	}
	assertLinked := func(label, body string) {
		t.Helper()
		if got := exactLineCount(body, linkedPRSignature); got != 1 {
			t.Errorf("%s: linked signature line appears %d times, want 1; body:\n%s", label, got, body)
		}
	}
	assertPlain := func(label, body string) {
		t.Helper()
		if got := exactLineCount(body, plainPRSignature); got != 1 {
			t.Errorf("%s: plain signature line appears %d times, want 1; body:\n%s", label, got, body)
		}
		if strings.Contains(body, signatureLinkURL) {
			t.Errorf("%s: body still links the no-mistakes project; body:\n%s", label, body)
		}
	}

	setTrustedConfig("", "trusted config without pr.signature")
	startRun("feature/sig-default", "")
	assertPlain("default, full appendix", publishedBody("feature/sig-default", "01-default-plain-full.md"))

	startRun("feature/sig-pushed-only", baseConfig+"pr:\n  signature: link\n")
	assertPlain("pushed-only link", publishedBody("feature/sig-pushed-only", "02-pushed-only-link-ignored.md"))

	setTrustedConfig("pr:\n  appendix: minimal\n", "trusted minimal appendix without pr.signature")
	startRun("feature/sig-default-minimal", "")
	assertPlain("default, minimal appendix", publishedBody("feature/sig-default-minimal", "03-default-plain-minimal.md"))

	// The template's top-level ## heading matches fakeagent's canned
	// "## Summary" body, so the structural heading check passes.
	h.CommitChange("main", ".github/pr_template.md", "## Summary\n\nDescribe the change.\n", "add PR template")
	setTrustedConfig("pr:\n  template: .github/pr_template.md\n  appendix: collapsed\n", "trusted template without pr.signature")
	startRun("feature/sig-default-template", "")
	templateBody := publishedBody("feature/sig-default-template", "04-default-plain-template-collapsed.md")
	assertPlain("default, template collapsed appendix", templateBody)
	if !strings.Contains(templateBody, "<!-- no-mistakes-pr-appendix:v1 ") {
		t.Errorf("template run did not render the owned template appendix; body:\n%s", templateBody)
	}

	setTrustedConfig("pr:\n  signature: link\n", "trusted pr.signature link")
	startRun("feature/sig-link", "")
	assertLinked("trusted link, full appendix", publishedBody("feature/sig-link", "05-trusted-link-full.md"))

	setTrustedConfig("pr:\n  signature: Plain\n", "trusted pr.signature invalid")
	out, err := axiRun("feature/sig-invalid", "")
	run := h.WaitForRun("feature/sig-invalid", 90*time.Second)
	saveSignatureEvidence(t, "06-trusted-invalid-run.txt", out+"\nstatus="+string(run.Status)+"\nerror="+deref(run.Error)+"\n")
	if err == nil {
		t.Errorf("axi run under an invalid pr.signature exited 0:\n%s", out)
	}
	if run.Status != types.RunFailed || !strings.Contains(deref(run.Error), "pr.signature") {
		t.Fatalf("invalid pr.signature: status=%s error=%q, want failed naming pr.signature", run.Status, deref(run.Error))
	}
	if pr := readStatefulGH(t, statePath).PRs["feature/sig-invalid"]; pr != nil {
		t.Fatalf("invalid pr.signature still published a PR:\n%s", pr.Body)
	}
}

func saveSignatureEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_SIGNATURE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}
