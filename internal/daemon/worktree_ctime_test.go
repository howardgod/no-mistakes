package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRebaseStepSurvivesACtimeOnlyChangeInTheRunWorktree reproduces the WSL
// drvfs failure: the filesystem moves a file's ctime after Git recorded its
// stat, and `git rebase` (which rereads the index and compares stat data only)
// then refuses with "Your local changes to the following files would be
// overwritten by checkout" although nothing is dirty. A pre-rebase hook waits
// past the next whole second and chmods, without changing the mode, the files
// the rebase must rewrite, which moves only their ctime inside that window.
// The run worktree is created by the daemon's own startup path, which must
// leave it with core.trustctime=false so the rebase succeeds. Without that
// setting this run fails at the rebase step with "would be overwritten by
// checkout".
func TestRebaseStepSurvivesACtimeOnlyChangeInTheRunWorktree(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&steps.RebaseStep{}}
	})
	const repoID = "ctime-drift-repo"
	repo, _ := setupTestGitRepo(t, p, d, repoID)
	gateDir := p.RepoDir(repoID)
	workDir := repo.WorkingPath
	// Mirror a real gate, where `no-mistakes init` enables per-worktree config.
	if err := git.IsolateHooksPath(t.Context(), gateDir); err != nil {
		t.Fatalf("isolate hooks path: %v", err)
	}

	baseFiles := []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt", "f5.txt"}
	for _, name := range append(baseFiles, "f40.txt") {
		writeTestFile(t, filepath.Join(workDir, name), "line\n")
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "add files")
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")

	gitCmd(t, workDir, "checkout", "-b", "feature")
	appendTestFile(t, filepath.Join(workDir, "f40.txt"), "feature change\n")
	gitCmd(t, workDir, "commit", "-am", "feature: touch f40")
	featureSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/feature")

	// The base moves on files the feature never touched, so the rebase must
	// rewrite exactly those files in the run worktree.
	gitCmd(t, workDir, "checkout", "main")
	for _, name := range baseFiles {
		appendTestFile(t, filepath.Join(workDir, name), "base change\n")
	}
	gitCmd(t, workDir, "commit", "-am", "base moved")
	movedBaseSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")

	// A linked worktree runs hooks from the gate's hooks directory, with the
	// worktree root as the working directory.
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "pre-rebase-ran"))
	hook := fmt.Sprintf("#!/bin/sh\nsleep 2\nchmod 644 %s\n: > '%s'\nexit 0\n", strings.Join(baseFiles, " "), marker)
	if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-rebase"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: gateDir,
		Ref:  "refs/heads/feature",
		Old:  strings.Repeat("0", 40),
		New:  featureSHA,
	}, &result); err != nil {
		t.Fatalf("push received: %v", err)
	}

	deadline := time.Now().Add(time.Minute)
	for {
		run, err := d.GetRun(result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunCompleted || run.Status == types.RunFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish: %+v", result.RunID, run)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pre-rebase hook did not run, so no ctime change was injected: %v", err)
	}
	stepResults, err := d.GetStepsByRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range stepResults {
		if sr.StepName == types.StepRebase && sr.Status != types.StepStatusCompleted {
			stepErr := ""
			if sr.Error != nil {
				stepErr = *sr.Error
			}
			t.Fatalf("rebase step status = %s, want completed; error: %s", sr.Status, stepErr)
		}
	}
	run, err := d.GetRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error: %v)", run.Status, run.Error)
	}
	if run.HeadSHA == featureSHA {
		t.Fatalf("run head is still the pushed head %s, want it rebased onto %s", featureSHA, movedBaseSHA)
	}
	gitCmd(t, "", "--git-dir="+gateDir, "merge-base", "--is-ancestor", movedBaseSHA, run.HeadSHA)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTestFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}
