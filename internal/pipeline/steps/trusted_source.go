package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// trustedSourceIsAlternate reports whether this run's trusted config came from
// an operator-authorized branch other than the registered default branch.
func trustedSourceIsAlternate(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Config != nil && sctx.Repo != nil &&
		sctx.Config.TrustedConfigBranch != "" && sctx.Config.TrustedConfigBranch != sctx.Repo.DefaultBranch
}

// trustedPromptSource names where trusted prompt policy came from, so an agent
// is never told "the default branch" for an operator-selected source.
func trustedPromptSource(sctx *pipeline.StepContext) string {
	if !trustedSourceIsAlternate(sctx) {
		return "the default branch"
	}
	return fmt.Sprintf("branch %q at %s", sctx.Config.TrustedConfigBranch, sctx.Config.TrustedConfigSHA)
}

// promptBaseBranch is the review prompts' "base branch" context value. An
// alternate trusted source is named there, where it costs nothing from the
// validated review.path_instructions budget.
func promptBaseBranch(sctx *pipeline.StepContext, baseBranch string) string {
	if !trustedSourceIsAlternate(sctx) {
		return baseBranch
	}
	return baseBranch + " (trusted config from " + trustedPromptSource(sctx) + ")"
}

// alternateSourcePathInstructionsHeading is the trusted path-instructions
// heading for an operator-selected source. The phrase it swaps in has the same
// length as "the default branch", so the byte budget the config was validated
// against still measures the section exactly; promptBaseBranch names the
// source branch and SHA.
var alternateSourcePathInstructionsHeading = strings.Replace(config.ReviewPathInstructionsHeading, "the default branch", "the trusted source", 1)

// trustedPathInstructionsHeading returns heading unchanged except for the
// trusted repository source of an alternate-source run. Operator sources never
// came from a branch, so their headings stay as they are.
func trustedPathInstructionsHeading(sctx *pipeline.StepContext, heading string) string {
	if heading == config.ReviewPathInstructionsHeading && trustedSourceIsAlternate(sctx) {
		return alternateSourcePathInstructionsHeading
	}
	return heading
}
