//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
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
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\nno_ci: true\npr:\n  template: .github/pull_request_template.md\n", "pin the repository PR template")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main: %v\n%s", err, out)
	}

	const branch = "feature/user-template"
	h.CommitChange(branch, "user-template.txt", "user template\n", "add user template fixture")
	if out, err := h.RunInDir(h.AddWorktree(branch), "axi", "run", "--intent", "exercise pr.user_template", "--skip", "ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	waitPublished(t, h, branch, "", branch)
	body := livePRBody(t, statePath, branch)
	saveUserTemplateEvidence(t, "pr-body.md", body)

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
