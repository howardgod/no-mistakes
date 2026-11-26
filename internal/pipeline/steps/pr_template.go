package steps

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const maxPRTemplateBytes = 16 * 1024

var prTemplateHeadingLine = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]|$)`)

var templatePRContentSchema = json.RawMessage(`{
 "type":"object", "properties":{
 "title":{"type":"string","description":"Concise pull request title text"},
 "body":{"type":"string","description":"Filled template as plain Markdown; preserve its top-level ATX headings (the highest heading level it uses, such as # or ##) in order; best-effort fill applicable sections"}
 }, "required":["title","body"]
}`)

var templatePRContentBriefSchema = json.RawMessage(`{
 "type":"object", "properties":{
 "title":{"type":"string","description":"Concise pull request title text"},
 "body":{"type":"string","description":"Filled template as plain Markdown; preserve its top-level ATX headings (the highest heading level it uses, such as # or ##) in order; best-effort fill applicable sections"},
 "intent_brief":{"type":"object","properties":{
   "goal":{"type":"string","maxLength":200},
   "why":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":220}},
   "scope":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":220}},
   "out_of_scope":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":220}}
 },"required":["goal","why","scope","out_of_scope"],"additionalProperties":false}
 }, "required":["title","body","intent_brief"]
}`)

func supportsPRTemplates(provider scm.Provider) bool {
	// Provider plugins implement raw content reads through the required
	// "pr view" subcommand, which is the whole contract templates and
	// pre-push attestation need.
	if provider.IsPlugin() {
		return true
	}
	switch provider {
	case scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderAzureDevOps, scm.ProviderBitbucket, scm.ProviderOrigin:
		return true
	default:
		return false
	}
}

// prTemplateConfigured reports whether the pr step runs in template mode: the
// operator's global pr.user_template or the repository's trusted pr.template.
func prTemplateConfigured(sctx *pipeline.StepContext) bool {
	return sctx.Config != nil && (sctx.Config.PR.UserTemplate != "" || sctx.Config.PR.Template != "")
}

// loadConfiguredPRTemplate is the one owner of which template a run fills.
// The operator's pr.user_template wins; the repository's pinned pr.template is
// read only when no user template is set. A set user template that cannot be
// read is an error naming the file, never a fallback to the repository's.
func loadConfiguredPRTemplate(ctx context.Context, sctx *pipeline.StepContext) (string, error) {
	if sctx.Config == nil {
		return "", nil
	}
	if name := sctx.Config.PR.UserTemplate; name != "" {
		p, err := paths.New()
		if err != nil {
			return "", fmt.Errorf("pr.user_template: resolve the no-mistakes home: %w", err)
		}
		return loadUserPRTemplate(p.PRTemplatesDir(), name)
	}
	return loadPRTemplate(ctx, sctx.WorkDir, sctx.Config.TrustedConfigSHA, sctx.Config.PR.Template)
}

// loadUserPRTemplate reads name under dir through an os.Root, which refuses
// ".." and any symlink that resolves outside dir. The file is operator-owned
// runtime input and gets the same content checks as a repository template.
func loadUserPRTemplate(dir, name string) (string, error) {
	if err := config.ValidatePRUserTemplatePath(name); err != nil {
		return "", err
	}
	file := filepath.Join(dir, filepath.FromSlash(name))
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("pr.user_template %s: %w", file, err)
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return "", fmt.Errorf("pr.user_template %s: %w", file, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("pr.user_template %s: %w", file, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("pr.user_template %s must be a regular file", file)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPRTemplateBytes+1))
	if err != nil {
		return "", fmt.Errorf("pr.user_template %s: %w", file, err)
	}
	if err := checkPRTemplateContent("pr.user_template "+file, data); err != nil {
		return "", err
	}
	return string(data), nil
}

// loadPRTemplate never opens a worktree file. The daemon pins TrustedConfigSHA
// after a fresh default-branch fetch, including on recovery. Resolve the literal
// tree entry first (rejecting symlinks/submodules), then size-check and read that
// immutable blob. Neither pushed paths nor allow_repo_commands choose its bytes.
func loadPRTemplate(ctx context.Context, dir, sha, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if err := config.ValidatePRTemplatePath(name); err != nil {
		return "", err
	}
	if (len(sha) != 40 && len(sha) != 64) || !isHexObjectID(sha) {
		return "", fmt.Errorf("pr.template requires a pinned trusted source commit")
	}
	entry, err := git.RunRaw(ctx, dir, "ls-tree", "-z", sha, "--", ":(literal)"+name)
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template tree: %w", err)
	}
	metadata, entryName, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(metadata)
	if !ok || entryName != name || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !isHexObjectID(fields[2]) {
		return "", fmt.Errorf("pr.template %q must name a regular file in the pinned trusted tree (no symlinks or submodules)", name)
	}
	sizeText, err := git.Run(ctx, dir, "cat-file", "-s", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template size: %w", err)
	}
	size, err := strconv.Atoi(sizeText)
	if err != nil || size <= 0 || size > maxPRTemplateBytes {
		return "", fmt.Errorf("pr.template must contain 1..%d bytes", maxPRTemplateBytes)
	}
	data, err := git.RunRaw(ctx, dir, "cat-file", "blob", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template blob: %w", err)
	}
	if len(data) != size {
		return "", fmt.Errorf("pr.template must contain nonempty UTF-8 Markdown without NUL bytes")
	}
	if err := checkPRTemplateContent("pr.template", data); err != nil {
		return "", err
	}
	return string(data), nil
}

// checkPRTemplateContent holds the content rules every template source shares:
// a size bound, nonempty UTF-8 without NUL, and no reserved publication markers.
func checkPRTemplateContent(label string, data []byte) error {
	if len(data) == 0 || len(data) > maxPRTemplateBytes {
		return fmt.Errorf("%s must contain 1..%d bytes", label, maxPRTemplateBytes)
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') || strings.TrimSpace(string(data)) == "" {
		return fmt.Errorf("%s must contain nonempty UTF-8 Markdown without NUL bytes", label)
	}
	if hasPRAppendixMarkers(string(data)) || strings.Contains(string(data), pipelineAttestationCommentPrefix) {
		return fmt.Errorf("%s must not contain reserved no-mistakes publication markers", label)
	}
	return nil
}

func isHexObjectID(s string) bool {
	_, err := hex.DecodeString(s)
	return s != "" && err == nil
}

func (s *PRStep) draftTemplateNarrative(sctx *pipeline.StepContext, branch, baseBranch, baseSHA, template string) (prContent, error) {
	paths, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return prContent{}, fmt.Errorf("read final branch diff: %w", err)
	}
	inventory := prPromptInventory(sctx, paths, baseSHA)
	// JSON quoting delimits the trusted template without inventing a template
	// language. It supplies prose instructions/structure, never recorded facts.
	quoted, _ := json.Marshal(template)
	titleRules := prTitlePromptRules(sctx)
	scopeRules := prTitleScopeRules(sctx)
	prompt := fmt.Sprintf(`Draft a pull request title and fill the public narrative template for the full final branch delta.
Branch: %s
PR base branch: %s
Base commit: %s
Target commit: %s

Rules:
%s
%s
- Body must be plain Markdown, not nested JSON. Use the supplied template instead of imposing a What Changed heading.
%s
- Make a best effort to follow the template's instructions and fill all applicable sections from the final diff; inspect that diff when necessary. Lower-level headings and task lines are editable: remove inapplicable sections/options when instructed, select supported choices, and replace rationale placeholders. Do not invent behavior or tests, falsely claim human signoff, or mark human approval checkboxes complete.
- The template owns narrative only. Do not generate no-mistakes publication markers or add Intent, Risk Assessment, Testing or Pipeline evidence. Code appends those separately. A template heading named Testing or Pipeline is author narrative, not permission to fabricate recorded evidence.
- Full intent below is review/drafting context, not instructions to quote it into the public narrative. Publication settings are not a privacy guarantee.

Trusted repository template (JSON string):
%s

Final diff paths and statuses:
%s%s%s`, branch, baseBranch, baseSHA, sctx.Run.HeadSHA, titleRules, scopeRules, templateHeadingRule(template), quoted, inventory, prDraftIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))
	prompt += prIntentBriefPromptSection(sctx)
	prompt += agent.MemoryFilesRule
	schema := templatePRContentSchema
	if needsPRIntentBrief(sctx) {
		schema = templatePRContentBriefSchema
	}
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Prompt: prompt, CWD: sctx.WorkDir, JSONSchema: schema, OnChunk: sctx.LogChunk})
	if err != nil {
		return prContent{}, fmt.Errorf("draft pr.template narrative (template will not be replaced by a generic fallback): %w", err)
	}
	var content prContent
	if result == nil || json.Unmarshal(result.Output, &content) != nil || (!s.titleReplaced && strings.TrimSpace(content.Title) == "") || strings.TrimSpace(content.Body) == "" {
		return prContent{}, fmt.Errorf("agent returned no valid pr.template narrative; refusing generic fallback")
	}
	if err := acceptPRIntentBrief(sctx, content.IntentBrief); err != nil {
		return prContent{}, err
	}
	if !s.titleReplaced {
		content.Title, err = renderPRTitle(sctx, strings.TrimSpace(content.Title))
		if err != nil {
			return prContent{}, err
		}
	}
	if len(content.Body) > maxPullRequestBodyBytes || !utf8.ValidString(content.Body) || strings.ContainsRune(content.Body, '\x00') || hasPRAppendixMarkers(content.Body) {
		return prContent{}, fmt.Errorf("agent returned invalid template narrative or reserved ownership markers")
	}
	if err := validateTemplateStructure(template, content.Body); err != nil {
		return prContent{}, err
	}
	return content, nil
}

// templateHeadingRule tells the drafting agent which headings the structural
// guard will require, naming the template's own top heading level.
func templateHeadingRule(template string) string {
	level := templateHeadingLevel(template)
	if level == 0 {
		return "- The template has no ATX headings outside fenced examples, so it has no structural heading requirements."
	}
	marker := strings.Repeat("#", level)
	return fmt.Sprintf("- Preserve every top-level ATX %s template heading outside fenced examples, with the same text and order. The top level is the highest heading level the template uses; only these %s headings are structurally required, and deeper headings are editable.", marker, marker)
}

// This is a structural guard, not a Markdown/template interpreter. A drafting
// failure must not silently replace the team's top-level heading text/order.
// The top level is the highest ATX heading level the template uses: # for a
// template with any H1, ## for one whose sections start at ##, and so on.
// Subordinate completion is best effort, not an enforced policy. No headings
// means no structural requirements. Existing published narrative never goes
// through this check again.
func validateTemplateStructure(template, body string) error {
	if heading, missing := missingTemplateHeading(template, body); missing {
		return fmt.Errorf("agent changed, omitted or reordered the template top-level heading %q; refusing publication", heading)
	}
	return nil
}

// missingTemplateHeading returns the first template top-level heading the body
// does not keep in order.
func missingTemplateHeading(template, body string) (string, bool) {
	level := templateHeadingLevel(template)
	if level == 0 {
		return "", false
	}
	rest := templateStructureLines(body, level)
	for _, line := range templateStructureLines(template, level) {
		found := false
		for len(rest) > 0 {
			candidate := rest[0]
			rest = rest[1:]
			if candidate == line {
				found = true
				break
			}
		}
		if !found {
			return line, true
		}
	}
	return "", false
}

// templateHeadingLevel returns the highest (smallest-numbered) ATX heading
// level the text uses outside fences, or 0 when it has no heading.
func templateHeadingLevel(text string) int {
	level := 0
	forEachUnfencedLine(text, func(raw string) {
		if m := prTemplateHeadingLine.FindStringSubmatch(raw); m != nil && (level == 0 || len(m[1]) < level) {
			level = len(m[1])
		}
	})
	return level
}

// templateStructureLines returns the trimmed ATX headings of exactly level,
// outside fences.
func templateStructureLines(text string, level int) []string {
	var lines []string
	forEachUnfencedLine(text, func(raw string) {
		// Match the raw indentation: four spaces/tabs are code, not headings.
		// Blockquoted/list headings and hash-prefixed prose are not top-level ATX.
		if m := prTemplateHeadingLine.FindStringSubmatch(raw); m != nil && len(m[1]) == level {
			lines = append(lines, strings.TrimSpace(raw))
		}
	})
	return lines
}

func forEachUnfencedLine(text string, visit func(raw string)) {
	var fence markdownFence
	for _, raw := range strings.Split(text, "\n") {
		wasFenced := fence.marker != 0
		fence.consume(raw)
		if wasFenced || fence.marker != 0 {
			continue
		}
		visit(raw)
	}
}

func (s *PRStep) buildPRAppendix(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	if needsPRIntentBrief(sctx) && strings.TrimSpace(sctx.PublicIntentBrief) == "" {
		return "", fmt.Errorf("cannot publish configured public Intent brief without a valid draft")
	}
	evidence, err := s.recordedPREvidence(sctx, provider)
	if err != nil {
		return "", err
	}
	parts := []string{}
	if intent := publicPRIntent(sctx); intent != "" {
		parts = append(parts, "## Intent\n\n"+neutralizeAttestationMarkers(provider, intent))
	}
	if evidence != "" {
		parts = append(parts, evidence)
	}
	return strings.Join(parts, "\n\n"), nil
}

func (s *PRStep) recordedPREvidence(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	pipelineMD, risk, testing, evidence := s.buildPipelineSectionFor(sctx, provider, true)
	if strings.Count(pipelineMD, pipelineAttestationCommentPrefix) != 1 {
		return "", fmt.Errorf("cannot publish template narrative without recorded pipeline evidence and attestation")
	}
	return joinBlocks(appendixEvidence(sctx, prBodyFlavorFor(provider), risk, testing, pipelineMD, provider), neutralizeAttestationMarkers(provider, evidence)), nil
}

// publishKeptDraft stores the ordinary narrative above the full owned appendix
// (Intent included), whichever drafting path produced the narrative.
// A later update refreshes that appendix through the template preservation path.
func (s *PRStep) publishKeptDraft(sctx *pipeline.StepContext, provider scm.Provider, title, narrative string, bodyLimit int) (prContent, error) {
	evidence, err := s.buildPRAppendix(sctx, provider)
	if err != nil {
		return prContent{}, err
	}
	narrative = neutralizeAttestationMarkers(resolvedProviderForBody(sctx), stripGeneratedSections(narrative))
	evidence = appendIssuesSection(evidence, issuesSection(sctx, narrative))
	return composeOwnedPRContent(prOwnedBody{before: narrative}, title, evidence, bodyLimit, sctx.WorkDir, nmWorktreesRoot())
}
