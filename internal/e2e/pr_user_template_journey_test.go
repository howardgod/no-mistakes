//go:build e2e

package e2e

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The operator's causal template: top-level sections at ##, no # heading,
// with ### subsections the drafter may drop. The headings mirror the
// operator's real runtime template; the file itself is never committed.
const userCausalTemplate = "## Summary\n\n- ...\n\n## Cause\n\n- ...\n\n## Trigger\n\n- ...\n\n## Effect (before this PR)\n\n- ...\n\n## Remedy (this PR)\n\n- ...\n\n### Facts the remedy relies on\n\n- ...\n\n## Review by intent\n\n1. ...\n\n## Related\n\n- ...\n\n## Risk\n\n- Level: <Low | Medium | High>\n\n## Test proof\n\n- ...\n\n## Verification layer check\n\n- NA\n\n## Gate results\n\n- NA\n"

var userCausalHeadings = []string{"## Summary", "## Cause", "## Trigger", "## Effect (before this PR)", "## Remedy (this PR)", "## Review by intent", "## Related", "## Risk", "## Test proof", "## Verification layer check", "## Gate results"}

const repoTLDRTemplate = "# TL;DR\n\n<!-- one line -->\n\n# Changes\n\n- ...\n"

// TestPRUserTemplateJourney drives pr.user_template through the real binary,
// daemon, gate hook, and stateful gh stub. The repository's trusted config
// pins a # TL;DR template; the global config names a causal template under
// <NM_HOME>/pr-templates. The drafting prompt must carry the causal template,
// and the published PR body must follow its ## headings in order.
//
// The trusted config also asks for the minimal appendix without the Intent
// section and still sets the removed pr.signature key, which must load and
// change nothing. The appendix
// is the risk line and the attestation between the hidden markers, with no
// signature. A later push onto a PR whose appendix still carries a signature
// (published before it was removed) must rewrite that appendix unsigned,
// keeping the author text.
func TestPRUserTemplateJourney(t *testing.T) {
	scenario := userTemplateScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "pr:\n  user_template: causal.md\n"})
	templates := filepath.Join(h.NMHome, "pr-templates")
	if err := os.MkdirAll(templates, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templates, "causal.md"), []byte(userCausalTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("main", ".github/pull_request_template.md", repoTLDRTemplate, "add repository PR template")
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\nno_ci: true\npr:\n  template: .github/pull_request_template.md\n  appendix: minimal\n  publish_intent: false\n  signature: link\n", "pin the repository PR template")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main: %v\n%s", err, out)
	}

	const branch = "feature/user-template"
	h.CommitChange(branch, "user-template.txt", "user template\n", "add user template fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "exercise pr.user_template", "--skip", "ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", branch)
	body := livePRBody(t, statePath, branch)
	saveUserTemplateEvidence(t, "pr-body.md", body)
	_, appendix, _ := userTemplateOwnedAppendix(t, body)
	assertUnsignedMinimalAppendix(t, "first publish", appendix)

	if strings.Contains(body, "# TL;DR") || strings.Contains(body, "# Changes") {
		t.Fatalf("published body used the repository template:\n%s", body)
	}
	// The causal headings must appear in order; the appendix may add more ## lines after them.
	lines := strings.Split(body, "\n")
	next := 0
	for _, line := range lines {
		if next < len(userCausalHeadings) && line == userCausalHeadings[next] {
			next++
		}
	}
	if next != len(userCausalHeadings) {
		t.Fatalf("published body is missing causal heading %q in order:\n%s", userCausalHeadings[next], body)
	}
	if !strings.Contains(body, "Filled from the operator's causal template") {
		t.Fatalf("published body is not the causal draft:\n%s", body)
	}

	// An appendix published before the signature line was removed:
	// same risk line and attestation, plus the plain signature line. The next
	// push must recognize that appendix as owned and rewrite it unsigned.
	before, appendix, after := userTemplateOwnedAppendix(t, body)
	risk, attestation, _ := strings.Cut(appendix, "\n\n")
	after += "\n\nAuthor note kept below the appendix.\n"
	signed := before + wrapUserTemplateAppendix(risk+"\n\n"+plainPRSignature+"\n\n"+attestation) + after
	state := readStatefulGH(t, statePath)
	state.PRs[branch].Body = signed
	writeStatefulGH(t, statePath, state)
	saveUserTemplateEvidence(t, "pr-body-signed-seed.md", signed)

	h.Checkout("main")
	h.RemoveWorktree(wt)
	h.CommitChange(branch, "user-template.txt", "user template v2\n", "update user template fixture")
	h.PushToGate(branch)
	second := waitPublished(t, h, branch, first.ID, "push onto a signed appendix")
	if second.ID == first.ID {
		t.Fatal("the second push did not start a new run")
	}
	updated := livePRBody(t, statePath, branch)
	saveUserTemplateEvidence(t, "pr-body-after-signed-update.md", updated)
	if strings.Count(updated, "<!-- no-mistakes-pr-appendix:v1 ") != 1 || strings.Count(updated, "<!-- /no-mistakes-pr-appendix:v1 -->") != 1 {
		t.Fatalf("the update duplicated the appendix:\n%s", updated)
	}
	gotBefore, gotAppendix, gotAfter := userTemplateOwnedAppendix(t, updated)
	if gotBefore != before || gotAfter != after {
		t.Fatalf("the update changed author text around the appendix:\n%s", updated)
	}
	assertUnsignedMinimalAppendix(t, "update of a signed appendix", gotAppendix)
	if gotAppendix == appendix {
		t.Fatalf("the update did not restamp the attestation for the new head:\n%s", updated)
	}
}

// userTemplateScenario answers the template drafting turn by which template
// reached the prompt: the causal one gets a causal fill, the repository's
// TL;DR one gets a TL;DR fill the test then rejects.
func userTemplateScenario(t *testing.T) string {
	t.Helper()
	fallback, err := os.ReadFile(cleanReviewScenario(t))
	if err != nil {
		t.Fatal(err)
	}
	var causal strings.Builder
	for _, heading := range userCausalHeadings {
		causal.WriteString(heading + "\n\n- Filled from the operator's causal template.\n\n")
	}
	content := "actions:\n"
	content += fmt.Sprintf("  - match: %q\n    structured:\n      title: %q\n      body: %q\n", "Trusted repository template (JSON string):\n\"## Summary\\n", "feat: causal user template", causal.String())
	content += fmt.Sprintf("  - match: %q\n    structured:\n      title: %q\n      body: %q\n", "Trusted repository template (JSON string):\n\"# TL;DR", "feat: repository template", "# TL;DR\n\nRepository template won.\n\n# Changes\n\n- x\n")
	content += strings.TrimPrefix(string(fallback), "actions:\n")
	path := filepath.Join(t.TempDir(), "user-template-scenario.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func saveUserTemplateEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_USER_TEMPLATE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}

var userTemplateAppendixPattern = regexp.MustCompile(`(?s)^(.*)<!-- no-mistakes-pr-appendix:v1 sha256=([0-9a-f]{64}) -->\n(.*)\n<!-- /no-mistakes-pr-appendix:v1 -->(.*)$`)

// userTemplateOwnedAppendix splits a published body around its owned
// appendix and checks the appendix still matches its recorded digest.
func userTemplateOwnedAppendix(t *testing.T, body string) (before, appendix, after string) {
	t.Helper()
	m := userTemplateAppendixPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("body has no owned appendix:\n%s", body)
	}
	if fmt.Sprintf("%x", sha256.Sum256([]byte(m[3]))) != m[2] {
		t.Fatalf("appendix digest does not match its content:\n%s", body)
	}
	return m[1], m[3], m[4]
}

func wrapUserTemplateAppendix(appendix string) string {
	return fmt.Sprintf("<!-- no-mistakes-pr-appendix:v1 sha256=%x -->\n%s\n<!-- /no-mistakes-pr-appendix:v1 -->", sha256.Sum256([]byte(appendix)), appendix)
}

// assertUnsignedMinimalAppendix checks the captain's layout: one visible risk
// line, then the hidden attestation, and no signature in either form.
func assertUnsignedMinimalAppendix(t *testing.T, label, appendix string) {
	t.Helper()
	risk, attestation, ok := strings.Cut(appendix, "\n\n")
	if !ok || risk == "" || strings.Contains(risk, "\n") || strings.HasPrefix(risk, "<!--") {
		t.Fatalf("%s: appendix does not open with a single risk line:\n%s", label, appendix)
	}
	if !strings.HasPrefix(attestation, "<!-- no-mistakes-pipeline-attestation:v1 {") || !strings.HasSuffix(attestation, "} -->") || strings.Contains(attestation, "\n") {
		t.Fatalf("%s: appendix holds more than the risk line and the attestation:\n%s", label, appendix)
	}
	if strings.Contains(appendix, "git push no-mistakes") {
		t.Fatalf("%s: appendix carries a signature:\n%s", label, appendix)
	}
}
