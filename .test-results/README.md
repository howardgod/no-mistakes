# Trusted Config Branch Gate Verification

- Tested code: the commit containing this evidence and the gate environment change, based on `2bf6f59813d6a9dbe77ae5b443207c247ba47878` (`origin/main`).
- Environment: Linux/amd64 (WSL2), Go 1.27.1, 2026-10-07 UTC.
- Rerun: `go test -tags=e2e ./internal/e2e -run '^TestTrustedConfigBranchJourney$' -count=1 -timeout=15m -v`
- Setup/reset: the E2E harness creates a fresh temporary upstream repository, run worktree, `NM_HOME`, fake agent and fake GitHub CLI per subtest; it cleans up its temporary daemon and state. No external forge or real agent was exercised.
- Expected: an authorized explicit `--base-branch verify` gate command echoes and checks `verify`; ordinary and unauthorized runs check `main` even if the PR base is `verify`; the recovered pinned run still checks `verify` after the source branch's gate declaration changes. The invalid/revoked source cases fail closed.
- Actual: all eight journey subtests passed, including recovery. Raw result: `trusted-config-branch-e2e.log`.
- Additional checks: `make lint`, `go test -race ./internal/pipeline/steps/...`, and `go build -o ./bin/no-mistakes ./cmd/no-mistakes` passed. The full `go test -race ./...` failed in two unrelated tests during concurrent execution: `TestMakeBuildIgnoresUnrelatedDotEnvEntries` (`make -n build` was killed) and `TestPushReceivedSkipStepsConfiguresExecutor` (the run missed its terminal-state timeout). Raw result: `go-test-race.log`. Both passed when rerun alone with `go test -race -p 1 . ./internal/daemon -run '^(TestMakeBuildIgnoresUnrelatedDotEnvEntries|TestPushReceivedSkipStepsConfiguresExecutor)$' -count=1 -timeout=5m -v`; raw result: `race-failure-rerun.log`. This does not make the original full-suite run a pass.
