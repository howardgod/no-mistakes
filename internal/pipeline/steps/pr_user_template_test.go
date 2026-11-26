package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// A user template's sections start at ## with no # heading, like the
// operator's causal template.
const testUserPRTemplate = "<!-- Fill every section. -->\n\n## Summary\n\n- ...\n\n## Cause\n\n- ...\n\n### Facts the remedy relies on\n\n- ...\n\n## Risk\n\n- Level: <Low | Medium | High>\n"
const filledUserPRTemplate = "## Summary\n\n- Add a Bar helper.\n\n## Cause\n\n- Callers had no Bar.\n\n## Risk\n\n- Level: Low\n"

// userTemplateHome points NM_HOME at a fresh directory and writes files under
// its pr-templates directory. It returns that directory.
func userTemplateHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	dir := filepath.Join(home, "pr-templates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPRUserTemplateWinsOverTheRepositoryTemplate(t *testing.T) {
	userTemplateHome(t, map[string]string{"causal.md": testUserPRTemplate})
	sctx, ag, _ := templateTestContext(t)
	sctx.Config.PR.UserTemplate = "causal.md"
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat(pipeline): fill user template", Body: filledUserPRTemplate})
		return &agent.Result{Output: data}, nil
	}
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("create: %+v, %v", out, err)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), filledUserPRTemplate) || strings.Contains(string(body), "# Overview") {
		t.Fatalf("published body does not follow the user template:\n%s", body)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	quoted, _ := json.Marshal(testUserPRTemplate)
	if !strings.Contains(prompt, string(quoted)) || strings.Contains(prompt, "# Overview") {
		t.Fatalf("prompt did not carry the user template instead of the repository's:\n%s", prompt)
	}
	if !strings.Contains(prompt, "only these ## headings are structurally required") {
		t.Fatalf("prompt does not name the user template's ## heading level:\n%s", prompt)
	}
}

func TestPRUserTemplateUnsetReadsTheRepositoryTemplate(t *testing.T) {
	userTemplateHome(t, map[string]string{"causal.md": testUserPRTemplate})
	sctx, _, _ := templateTestContext(t)
	got, err := loadConfiguredPRTemplate(sctx.Ctx, sctx)
	if err != nil || got != testPRTemplate {
		t.Fatalf("template = %q, err %v; want the repository's pinned template", got, err)
	}
	sctx.Config.PR.Template = ""
	if prTemplateConfigured(sctx) {
		t.Fatal("template mode on with neither template configured")
	}
	sctx.Config.PR.UserTemplate = "causal.md"
	if !prTemplateConfigured(sctx) {
		t.Fatal("a user template alone must turn template mode on")
	}
	got, err = loadConfiguredPRTemplate(sctx.Ctx, sctx)
	if err != nil || got != testUserPRTemplate {
		t.Fatalf("template = %q, err %v; want the user template with no repository template", got, err)
	}
}

func TestPRUserTemplateMissingFileFailsNamingItWithoutFallback(t *testing.T) {
	dir := userTemplateHome(t, nil)
	sctx, ag, _ := templateTestContext(t)
	sctx.Config.PR.UserTemplate = "causal.md"
	env, _ := fakeGH(t, "")
	sctx.Env = env
	_, err := (&PRStep{}).Execute(sctx)
	want := filepath.Join(dir, "causal.md")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one naming %s", err, want)
	}
	if len(ag.calls) != 0 {
		t.Fatal("a missing user template fell back to drafting with the repository template")
	}
}

func TestPRUserTemplateRefusesPathsOutsideTheTemplatesDirectory(t *testing.T) {
	dir := userTemplateHome(t, map[string]string{"nested/ok.md": testUserPRTemplate})
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte(testUserPRTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := loadUserPRTemplate(dir, "nested/ok.md"); err != nil || got != testUserPRTemplate {
		t.Errorf("nested/ok.md = %q, err %v; want the template", got, err)
	}
	for _, name := range []string{outside, "../outside.md", "nested/../ok.md", "./ok.md", `C:\x.md`, `nested\ok.md`, "nested"} {
		if _, err := loadUserPRTemplate(dir, name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}

	if err := os.Symlink(outside, filepath.Join(dir, "escape.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash("nested/ok.md"), filepath.Join(dir, "inside.md")); err != nil {
		t.Fatal(err)
	}
	if got, err := loadUserPRTemplate(dir, "inside.md"); err != nil || got != testUserPRTemplate {
		t.Errorf("inside.md = %q, err %v; want the template a symlink inside the directory names", got, err)
	}
	for _, name := range []string{"escape.md", "escape-dir/outside.md"} {
		if _, err := loadUserPRTemplate(dir, name); err == nil {
			t.Errorf("%q escaped the templates directory through a symlink", name)
		}
	}
}

func TestPRUserTemplateKeepsTheContentChecks(t *testing.T) {
	dir := userTemplateHome(t, map[string]string{
		"empty.md":    "",
		"blank.md":    "  \n",
		"nul.md":      "## A\x00",
		"invalid.md":  "## A\xff",
		"large.md":    "## A\n" + strings.Repeat("x", maxPRTemplateBytes),
		"marker.md":   "## A\n" + pipelineAttestationCommentPrefix,
		"appendix.md": "## A\n<!-- no-mistakes-pr-appendix:v1 x -->",
	})
	for _, name := range []string{"empty.md", "blank.md", "nul.md", "invalid.md", "large.md", "marker.md", "appendix.md"} {
		_, err := loadUserPRTemplate(dir, name)
		if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, name)) {
			t.Errorf("%s: error = %v, want a refusal naming the file", name, err)
		}
	}
}
