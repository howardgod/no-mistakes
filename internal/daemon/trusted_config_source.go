package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// trustedSourceForRun preserves the source selected at creation. An old run
// without a pin always uses the default branch, even after an operator opts in.
func trustedSourceForRun(global *config.GlobalConfig, repo *db.Repo, run *db.Run) (string, error) {
	if run.TrustedConfigBranch == nil {
		return repo.DefaultBranch, nil
	}
	branch := *run.TrustedConfigBranch
	if branch == "" || run.PRBaseBranch == nil || *run.PRBaseBranch != branch || !global.TrustedConfigBranchAllowed(repo.UpstreamURL, branch) {
		return "", fmt.Errorf("trusted config branch %q for run %s is no longer authorized for repository %s and its explicit PR base", branch, run.ID, safeurl.Redact(repo.UpstreamURL))
	}
	return branch, nil
}

// trustedFetch names the three callers of resolveTrustedSource, because each
// one fetches the source branch differently.
type trustedFetch int

const (
	// fetchForStart updates the run worktree's origin tracking ref, or fetches
	// the refreshed registration URL into that ref when the gate's origin is stale.
	fetchForStart trustedFetch = iota
	// fetchForRecovery fetches the worktree's own origin through the bounded
	// recovery seam, as recovery always has.
	fetchForRecovery
	// fetchForPiPreflight imports into a private ref so a refused Pi profile
	// cannot perturb the refs of an in-flight run it must not supersede.
	fetchForPiPreflight
)

// resolveTrustedSource is the single fetch/resolve/read path for run start,
// recovery and the Pi preflight: fresh fetch, pinned SHA, readability check,
// parse. A failed fetch never reads an old tracking ref and never falls back to
// another branch.
func resolveTrustedSource(ctx context.Context, dir string, repo *db.Repo, branch, runID string, mode trustedFetch) (string, *config.RepoConfig, error) {
	if strings.TrimSpace(branch) == "" {
		return "", nil, assertGateTrustedConfigReadable(ctx, dir, "", "")
	}
	ref := "refs/remotes/origin/" + branch
	originURL, urlErr := git.GetRemoteURL(ctx, dir, "origin")
	refreshed := repo.URLsVerified && (urlErr != nil || safeurl.Redact(originURL) != repo.UpstreamURL)
	var err error
	switch {
	case mode == fetchForPiPreflight:
		ref = fmt.Sprintf("refs/no-mistakes/pi-profile/%d-%d", os.Getpid(), time.Now().UnixNano())
		defer func() {
			_, _ = git.Run(context.WithoutCancel(ctx), dir, "update-ref", "--no-deref", "-d", ref)
		}()
		remote := "origin"
		if refreshed {
			remote = repo.UpstreamURL
		}
		err = git.FetchRemoteBranchToPrivateRef(ctx, dir, remote, branch, ref)
	case mode == fetchForRecovery:
		err = fetchRecoveredRemoteBranch(ctx, dir, "origin", branch)
	case refreshed:
		err = git.FetchRemoteBranchToRef(ctx, dir, repo.UpstreamURL, branch, ref)
	default:
		err = git.FetchRemoteBranch(ctx, dir, "origin", branch)
	}
	if err != nil {
		return "", nil, fmt.Errorf("cannot evaluate disable_project_settings: failed to fetch trusted source branch %q (refusing to run without reading the trusted config): %w", branch, err)
	}
	sha, err := git.ResolveRef(ctx, dir, ref)
	if err != nil {
		return "", nil, fmt.Errorf("cannot evaluate disable_project_settings: failed to resolve trusted source branch %q: %w", branch, err)
	}
	if err := assertGateTrustedConfigReadable(ctx, dir, branch, sha); err != nil {
		return "", nil, err
	}
	return sha, loadTrustedRepoConfig(ctx, dir, sha, runID), nil
}
