//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// plainPRSignature is the line an appendix carried before the signature was
// removed. Journeys seed it into existing PR bodies; nothing writes it.
const plainPRSignature = "Updates from git push no-mistakes"

// TestPRNoSignatureJourney drives PR publication through the real binary,
// daemon, gate hook, and the stateful gh stub, reading each PR body back as
// published. No appendix carries a signature line, whatever the appendix mode
// or narrative source, and a trusted config that still sets the removed
// pr.signature key - including a value the old validation refused - keeps
// running instead of failing.
func TestPRNoSignatureJourney(t *testing.T) {
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
	publish := func(branch, evidenceName string) string {
		t.Helper()
		h.CommitChange(branch, strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n", "add "+branch)
		if out, err := h.RunInDir(h.AddWorktree(branch), "axi", "run", "--intent", "exercise the unsigned appendix", "--skip", "ci"); err != nil {
			t.Fatalf("axi run %s: %v\n%s", branch, err, out)
		}
		run := waitPublished(t, h, branch, "", branch)
		if run.Status == types.RunFailed {
			t.Fatalf("%s: run failed: %s", branch, deref(run.Error))
		}
		body := livePRBody(t, statePath, branch)
		saveSignatureEvidence(t, evidenceName, body)
		return body
	}
	assertUnsigned := func(label, body string) {
		t.Helper()
		if strings.Contains(body, "git push no-mistakes") {
			t.Errorf("%s: body carries a signature line; body:\n%s", label, body)
		}
		if strings.Count(body, "<!-- no-mistakes-pipeline-attestation:v1 {") != 1 {
			t.Errorf("%s: body does not carry exactly one attestation; body:\n%s", label, body)
		}
	}

	setTrustedConfig("", "trusted config with the default appendix")
	assertUnsigned("full appendix", publish("feature/unsigned-full", "01-full.md"))

	setTrustedConfig("pr:\n  appendix: minimal\n  signature: link\n", "trusted minimal appendix with the removed pr.signature: link")
	minimal := publish("feature/unsigned-minimal", "02-minimal-retired-link.md")
	assertUnsigned("minimal appendix, retired pr.signature: link", minimal)
	if lines := strings.Split(strings.TrimRight(minimal, "\n"), "\n"); len(lines) < 3 || !strings.HasPrefix(lines[len(lines)-1], "<!-- no-mistakes-pipeline-attestation:v1 {") || lines[len(lines)-2] != "" || strings.HasPrefix(lines[len(lines)-3], "<!--") {
		t.Errorf("minimal tail is not the risk line followed by the attestation; body:\n%s", minimal)
	}

	h.CommitChange("main", ".github/pr_template.md", "## Overview\n\nDescribe the change.\n", "add PR template")
	setTrustedConfig("pr:\n  template: .github/pr_template.md\n  appendix: collapsed\n  signature: Plain\n", "trusted template with a formerly invalid pr.signature")
	templateBody := publish("feature/unsigned-template", "03-template-collapsed-retired-invalid.md")
	assertUnsigned("template collapsed appendix, retired pr.signature: Plain", templateBody)
	if !strings.Contains(templateBody, "<!-- no-mistakes-pr-appendix:v1 ") {
		t.Errorf("template run did not render the owned template appendix; body:\n%s", templateBody)
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
