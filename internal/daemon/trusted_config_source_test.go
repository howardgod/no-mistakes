package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// trustedSourceGate builds a bare gate that is its own origin, with main and
// verify carrying different trusted configs, and a linked run worktree. Real
// Git throughout; nothing is faked. These cases cannot be driven reliably
// through the e2e harness because they need the shared tracking ref left stale
// at the moment the fetch fails.
func trustedSourceGate(t *testing.T, verifyConfig string) (bare, wt string) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, src, "add", ".")
	}
	write("commands:\n  lint: \"echo main-lint\"\n")
	gitCmd(t, src, "commit", "-m", "main policy")
	gitCmd(t, src, "checkout", "-b", "verify")
	write(verifyConfig)
	gitCmd(t, src, "commit", "-m", "verify policy")

	bare = filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "main", "verify")
	wt = filepath.Join(t.TempDir(), "wt")
	if err := git.WorktreeAdd(ctx, bare, wt, gitOutput(t, src, "rev-parse", "main")); err != nil {
		t.Fatal(err)
	}
	return bare, wt
}

func TestResolveTrustedSource_ReadsOnlyTheSelectedBranchAtItsFetchedSHA(t *testing.T) {
	ctx := context.Background()
	bare, wt := trustedSourceGate(t, "commands:\n  lint: \"echo verify-lint\"\n")
	sha, cfg, err := resolveTrustedSource(ctx, wt, &db.Repo{DefaultBranch: "main"}, "verify", "run", fetchForStart)
	if err != nil {
		t.Fatal(err)
	}
	if want := gitOutput(t, bare, "rev-parse", "refs/heads/verify"); sha != want {
		t.Fatalf("pinned SHA = %s, want verify tip %s", sha, want)
	}
	if cfg == nil || cfg.Commands.Lint != "echo verify-lint" {
		t.Fatalf("trusted config = %+v, want verify's", cfg)
	}
}

func TestResolveTrustedSource_MissingBranchNeverReadsItsStaleTrackingRef(t *testing.T) {
	ctx := context.Background()
	bare, wt := trustedSourceGate(t, "commands:\n  lint: \"echo verify-lint\"\n")
	repo := &db.Repo{DefaultBranch: "main"}
	for _, mode := range []trustedFetch{fetchForStart, fetchForRecovery, fetchForPiPreflight} {
		// A previous run left origin/verify behind; then verify was deleted.
		if err := git.FetchRemoteBranch(ctx, wt, "origin", "verify"); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, bare, "update-ref", "-d", "refs/heads/verify")
		sha, cfg, err := resolveTrustedSource(ctx, wt, repo, "verify", "run", mode)
		if err == nil || sha != "" || cfg != nil {
			t.Fatalf("mode %d: sha=%q cfg=%+v err=%v; want a refusal, not the stale tracking ref or main", mode, sha, cfg, err)
		}
		if !strings.Contains(err.Error(), `failed to fetch trusted config source branch "verify"`) {
			t.Fatalf("mode %d: error does not name the source branch: %v", mode, err)
		}
		gitCmd(t, bare, "update-ref", "refs/heads/verify", "refs/remotes/origin/verify")
	}
}

func TestResolveTrustedSource_UnparseableSourceConfigAborts(t *testing.T) {
	_, wt := trustedSourceGate(t, "gates: [unterminated\n")
	_, _, err := resolveTrustedSource(context.Background(), wt, &db.Repo{DefaultBranch: "main"}, "verify", "run", fetchForStart)
	if err == nil || !strings.Contains(err.Error(), `trusted source branch "verify"`) || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("err = %v, want an abort naming verify's unparseable config", err)
	}
}

func TestTrustedSourceForRun_LegacyPinnedAndRevoked(t *testing.T) {
	const remote = "git@github.com:acme/widget.git"
	optedIn, err := config.LoadGlobalFromBytes([]byte("repository_overrides:\n  " + remote + ":\n    trusted_config_branches: [verify]\n"))
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := config.LoadGlobalFromBytes([]byte("repository_overrides:\n  " + remote + ":\n    pr:\n      title_format: '{{.Title}}'\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo := &db.Repo{DefaultBranch: "main", UpstreamURL: remote}
	verify, master := "verify", "main"
	for _, tc := range []struct {
		name    string
		global  *config.GlobalConfig
		run     *db.Run
		want    string
		wantErr bool
	}{
		// A run created before the opt-in keeps the default branch even now.
		{"legacy_run_after_opt_in", optedIn, &db.Run{ID: "r", PRBaseBranch: &verify}, "main", false},
		{"pinned_and_still_authorized", optedIn, &db.Run{ID: "r", PRBaseBranch: &verify, TrustedConfigBranch: &verify}, "verify", false},
		{"pinned_but_opt_in_revoked", revoked, &db.Run{ID: "r", PRBaseBranch: &verify, TrustedConfigBranch: &verify}, "", true},
		{"pinned_but_pr_base_differs", optedIn, &db.Run{ID: "r", PRBaseBranch: &master, TrustedConfigBranch: &verify}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustedSourceForRun(tc.global, repo, tc.run)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("source = %q, err = %v; want %q (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
