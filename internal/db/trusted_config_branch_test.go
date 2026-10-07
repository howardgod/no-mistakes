package db

import (
	"path/filepath"
	"testing"
)

func TestTrustedConfigBranchPinSurvivesRunRead(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:acme/widget.git", "master")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRunWithTrustedConfigBranch(repo.ID, "feature", "head", "base", nil, "", "", "", "verify", "verify", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrustedConfigBranch == nil || *got.TrustedConfigBranch != "verify" || got.PRBaseBranch == nil || *got.PRBaseBranch != "verify" {
		t.Fatalf("run lost independent source and PR pins: %+v", got)
	}
	legacy, err := d.InsertRun(repo.ID, "other", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	got, err = d.GetRun(legacy.ID)
	if err != nil || got.TrustedConfigBranch != nil {
		t.Fatalf("legacy source pin = %v, error = %v", got.TrustedConfigBranch, err)
	}
}

func TestOpenMigratesTrustedConfigBranchAsLegacyDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN trusted_config_branch`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for range 2 {
		d, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.GetRun(run.ID)
		if err != nil || got.TrustedConfigBranch != nil {
			t.Fatalf("migrated legacy run = %+v, error = %v; want default-branch source", got, err)
		}
		d.Close()
	}
}
