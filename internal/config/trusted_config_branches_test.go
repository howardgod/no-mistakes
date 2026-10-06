package config

import (
	"strings"
	"testing"
)

func TestTrustedConfigBranches_OnlyExplicitExactRemoteAndBranch(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  git@github.com:acme/widget.git:
    trusted_config_branches: [verify]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remote, explicitBase, want string
	}{
		{"https://github.com/ACME/widget", "verify", "verify"},
		{"git@github.com:acme/widget.git", "", ""},
		{"git@github.com:acme/widget.git", "master", ""},
		{"git@github.com:acme/other.git", "verify", ""},
		{"git@github.com:other/widget.git", "verify", ""},
	} {
		if got := cfg.TrustedConfigBranch(tc.remote, tc.explicitBase, "master"); got != tc.want {
			t.Errorf("TrustedConfigBranch(%q, %q) = %q, want %q", tc.remote, tc.explicitBase, got, tc.want)
		}
	}
}

func TestTrustedConfigBranches_RejectInvalidAndPushedSettings(t *testing.T) {
	for _, branch := range []string{"''", "' verify'", "'bad..name'", "'verify verify'", "'@{bad}'"} {
		_, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  git@github.com:acme/widget.git:\n    trusted_config_branches: [" + branch + "]\n"))
		if err == nil || !strings.Contains(err.Error(), "trusted_config_branches") {
			t.Errorf("invalid branch %s: error = %v", branch, err)
		}
	}
	// Repo YAML has no source selector; its unknown key is inert even if the
	// contributor includes it alongside an explicit pr.base_branch.
	if _, err := LoadRepoFromBytes([]byte("trusted_config_branches: [verify]\npr:\n  base_branch: verify\n")); err != nil {
		t.Fatalf("pushed source key should remain inert: %v", err)
	}
}
