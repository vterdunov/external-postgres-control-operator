# Remove KEEP_SECRET_NAME — Always Use spec.secretName

## TL;DR

> **Quick Summary**: Remove the global `KEEP_SECRET_NAME` env var entirely. Secret name is always `spec.secretName` from PostgresUser CR — no suffix, no flags.
> 
> **Deliverables**:
> - Remove `KEEP_SECRET_NAME` env var parsing and `KeepSecretName` config field
> - Simplify secret naming logic in controller
> - Update all tests (unit + e2e assertions)
> - Update deployment manifest and documentation
> 
> **Estimated Effort**: Quick
> **Parallel Execution**: YES - 2 waves
> **Critical Path**: Task 1 → Task 2 → Task 3 → Task 4

---

## Context

### Original Request
Remove global `KEEP_SECRET_NAME` environment variable. Make the secret name always equal to `spec.secretName` from PostgresUser CR. Currently, when `KEEP_SECRET_NAME=false` (default), secret name is `{secretName}-{crName}`. User wants the behavior of `KEEP_SECRET_NAME=true` to become the only behavior, unconditionally.

### Interview Summary
**Key Discussions**:
- Default behavior change: secret name always = `spec.secretName`, no `-{crName}` suffix
- Breaking change: acceptable, no migration path needed
- No new CR field: this is pure removal, not replacement

**Research Findings**:
- `keepSecretName` used in exactly one place: `newSecretForCR()` method at line 342-345
- Tests reference `keepSecretName` in 5 places (4× false, 1× true)
- E2E test `02-assert.yaml` hardcodes `my-secret-my-db-user` (old format)
- Helm chart has zero references — no chart changes needed

### Metis Review
**Identified Gaps** (addressed):
- E2E assertion file `02-assert.yaml` needs updating (was not in original scope)
- `strconv` import in `config.go` may become unused after removal — must check and clean
- Secret name collision risk documented (two PostgresUser CRs with same secretName in same namespace) — accepted tradeoff

---

## Work Objectives

### Core Objective
Eliminate the `KEEP_SECRET_NAME` feature flag. Secret name is always `spec.secretName`.

### Concrete Deliverables
- `pkg/config/config.go` — `KeepSecretName` field and parsing removed
- `internal/controller/postgresuser_controller.go` — `keepSecretName` field removed, `name := cr.Spec.SecretName` unconditionally
- `internal/controller/postgresuser_controller_test.go` — all `keepSecretName` references removed, assertions updated
- `config/manager/operator.yaml` — `KEEP_SECRET_NAME` env var removed
- `tests/e2e/basic-operations/02-assert.yaml` — secret name assertion updated
- `README.md` — all `KEEP_SECRET_NAME` documentation removed

### Definition of Done
- [ ] `grep -rn "keepSecretName\|KeepSecretName\|KEEP_SECRET_NAME\|keep_secret_name" --include="*.go" --include="*.yaml" --include="*.md" .` returns zero matches
- [ ] `make test` passes (all unit tests green)
- [ ] `go build ./...` succeeds with no errors

### Must Have
- Secret name always equals `spec.secretName` — no suffix appended
- All references to `KEEP_SECRET_NAME` / `keepSecretName` / `KeepSecretName` removed from entire repo
- Unit tests updated and passing
- E2E assertion updated

### Must NOT Have (Guardrails)
- Do NOT add any new configuration field, flag, or env var
- Do NOT modify `newSecretForCR` signature — only the body (name assignment)
- Do NOT touch secret data construction logic (URLs, template rendering, etc.)
- Do NOT modify CRD types (`api/v1alpha1/postgresuser_types.go`) — `secretName` field stays as-is
- Do NOT modify Helm chart (no references exist)
- Do NOT add migration or backward-compatibility logic
- Do NOT add validation for duplicate secretNames (out of scope)

---

## Verification Strategy

> **ZERO HUMAN INTERVENTION** — ALL verification is agent-executed. No exceptions.

### Test Decision
- **Infrastructure exists**: YES (Go tests with gomega/ginkgo, envtest)
- **Automated tests**: YES (tests-after — update existing tests to match new behavior)
- **Framework**: `go test` via `make test`

### QA Policy
Every task MUST include agent-executed QA scenarios.
Evidence saved to `.sisyphus/evidence/task-{N}-{scenario-slug}.{ext}`.

- **Go code**: Use Bash — `go build ./...`, `make test`, grep for zero remaining references
- **YAML/Docs**: Use Bash (grep) — verify no stale references

---

## Execution Strategy

### Parallel Execution Waves

```
Wave 1 (Start Immediately — Go code changes, independent files):
├── Task 1: Remove KeepSecretName from config.go [quick]
├── Task 2: Remove keepSecretName from controller + simplify logic [quick]
└── Task 3: Update unit tests [quick]

Wave 2 (After Wave 1 — YAML, docs, final verification):
├── Task 4: Update operator.yaml + e2e assertion + README [quick]
└── Task 5: Final verification — zero references + tests pass [quick]

Critical Path: Task 1,2 → Task 3 → Task 4 → Task 5
Parallel Speedup: Tasks 1+2 in parallel, then 3, then 4+5
```

### Dependency Matrix

| Task | Depends On | Blocks |
|------|-----------|--------|
| 1    | —         | 3, 5   |
| 2    | —         | 3, 5   |
| 3    | 1, 2      | 5      |
| 4    | —         | 5      |
| 5    | 1, 2, 3, 4| —      |

### Agent Dispatch Summary

- **Wave 1**: 3 tasks — T1 → `quick`, T2 → `quick`, T3 → `quick`
- **Wave 2**: 2 tasks — T4 → `quick`, T5 → `quick`

---

## TODOs

- [x] 1. Remove `KeepSecretName` from config

  **What to do**:
  - In `pkg/config/config.go`: Remove `KeepSecretName bool` field from `Cfg` struct (line 21)
  - Remove the `if value, err := strconv.ParseBool(...)` block (lines 49-51) that parses `KEEP_SECRET_NAME`
  - Check if `strconv` import is still used elsewhere in the file — if not, remove it

  **Must NOT do**:
  - Do NOT modify any other fields in `Cfg` struct
  - Do NOT change how other env vars are parsed

  **Recommended Agent Profile**:
  - **Category**: `quick`
    - Reason: Single file, 3 lines removed, trivial change
  - **Skills**: []
  - **Skills Evaluated but Omitted**:
    - None applicable

  **Parallelization**:
  - **Can Run In Parallel**: YES
  - **Parallel Group**: Wave 1 (with Tasks 2, 3)
  - **Blocks**: Task 3, Task 5
  - **Blocked By**: None (can start immediately)

  **References**:

  **Pattern References**:
  - `pkg/config/config.go:21` — `KeepSecretName bool` field to remove
  - `pkg/config/config.go:49-51` — `strconv.ParseBool` block to remove

  **WHY Each Reference Matters**:
  - Line 21 is the struct field definition — removing it ensures the config no longer carries this value
  - Lines 49-51 are the env var parsing — removing them ensures the env var is no longer read
  - `strconv` import (line 5) may become unused — Go compiler will error if left

  **Acceptance Criteria**:

  **QA Scenarios (MANDATORY):**

  ```
  Scenario: Config struct no longer has KeepSecretName field
    Tool: Bash (grep)
    Preconditions: File edited
    Steps:
      1. Run: grep -n "KeepSecretName" pkg/config/config.go
      2. Assert: zero matches
    Expected Result: No output (zero matches)
    Failure Indicators: Any line containing "KeepSecretName"
    Evidence: .sisyphus/evidence/task-1-config-no-keepsecretnme.txt

  Scenario: Config file compiles cleanly
    Tool: Bash
    Preconditions: File edited
    Steps:
      1. Run: go build ./pkg/config/...
      2. Assert: exit code 0, no output
    Expected Result: Clean compilation
    Failure Indicators: Any compiler error, especially "imported and not used: strconv"
    Evidence: .sisyphus/evidence/task-1-config-builds.txt
  ```

  **Commit**: YES (groups with Tasks 2, 3, 4 in single commit)
  - Message: `refactor: remove KEEP_SECRET_NAME; always use spec.secretName as secret name`
  - Files: `pkg/config/config.go`

- [x] 2. Remove `keepSecretName` from controller and simplify secret naming

  **What to do**:
  - In `internal/controller/postgresuser_controller.go`:
    - Remove `keepSecretName bool` field from `PostgresUserReconciler` struct (line 36) and its comment
    - Remove `keepSecretName: cfg.KeepSecretName,` from `NewPostgresUserReconciler` constructor (line 50)
    - Replace lines 342-345 (the conditional `name` assignment) with just: `name := cr.Spec.SecretName`

  **Must NOT do**:
  - Do NOT modify `newSecretForCR` function signature
  - Do NOT touch secret data construction (URLs, template rendering, etc.)
  - Do NOT change any other fields in the reconciler struct

  **Recommended Agent Profile**:
  - **Category**: `quick`
    - Reason: Single file, ~6 lines changed, straightforward removal
  - **Skills**: []

  **Parallelization**:
  - **Can Run In Parallel**: YES
  - **Parallel Group**: Wave 1 (with Tasks 1, 3)
  - **Blocks**: Task 3, Task 5
  - **Blocked By**: None (can start immediately)

  **References**:

  **Pattern References**:
  - `internal/controller/postgresuser_controller.go:36` — `keepSecretName bool` field to remove
  - `internal/controller/postgresuser_controller.go:50` — constructor assignment to remove
  - `internal/controller/postgresuser_controller.go:342-345` — conditional logic to simplify

  **WHY Each Reference Matters**:
  - Line 36 is the struct field — removing it breaks the compile if any code still references it
  - Line 50 connects config to controller — must be removed alongside config field
  - Lines 342-345 contain the actual branching logic: `if r.keepSecretName { name = cr.Spec.SecretName }` — replace entire block with `name := cr.Spec.SecretName`

  **Acceptance Criteria**:

  **QA Scenarios (MANDATORY):**

  ```
  Scenario: Controller no longer references keepSecretName
    Tool: Bash (grep)
    Preconditions: File edited
    Steps:
      1. Run: grep -n "keepSecretName\|KeepSecretName" internal/controller/postgresuser_controller.go
      2. Assert: zero matches
    Expected Result: No output (zero matches)
    Failure Indicators: Any line containing keepSecretName
    Evidence: .sisyphus/evidence/task-2-controller-no-keepsecretnme.txt

  Scenario: Secret name logic is unconditional
    Tool: Bash (grep)
    Preconditions: File edited
    Steps:
      1. Run: grep -A1 "name :=" internal/controller/postgresuser_controller.go | grep -i "secretname"
      2. Assert: shows `name := cr.Spec.SecretName` (no conditional, no fmt.Sprintf)
    Expected Result: Single line with `name := cr.Spec.SecretName`
    Failure Indicators: Conditional logic, fmt.Sprintf with suffix
    Evidence: .sisyphus/evidence/task-2-name-assignment.txt
  ```

  **Commit**: YES (groups with Tasks 1, 3, 4 in single commit)
  - Files: `internal/controller/postgresuser_controller.go`

- [x] 3. Update unit tests

  **What to do**:
  - In `internal/controller/postgresuser_controller_test.go`:
    - Remove ALL `rp.keepSecretName = false` assignments (lines ~709, ~758, ~824)
    - Remove ALL `rp.keepSecretName = true` assignments (line ~795)
    - DELETE the entire test case `"should respect keepSecretName setting when true"` (lines ~792-820) — this test is meaningless without the flag
    - Update assertion at line ~744: `Expect(secret.Name).To(Equal("mysecret-myuser"))` → `Expect(secret.Name).To(Equal("mysecret"))`
    - Update assertion at line ~788: `Expect(secret.Name).To(Equal("mysecret2-myuser2"))` → `Expect(secret.Name).To(Equal("mysecret2"))`
    - In the template test (~line 822+): remove `rp.keepSecretName = false` line

  **Must NOT do**:
  - Do NOT change test logic unrelated to keepSecretName
  - Do NOT modify assertions about secret data, labels, or annotations
  - Do NOT add new tests — just update existing ones

  **Recommended Agent Profile**:
  - **Category**: `quick`
    - Reason: Single file, mechanical updates to test assertions
  - **Skills**: []

  **Parallelization**:
  - **Can Run In Parallel**: NO (depends on Tasks 1+2 for compilation)
  - **Parallel Group**: Wave 1 (after Tasks 1+2 complete)
  - **Blocks**: Task 5
  - **Blocked By**: Task 1, Task 2

  **References**:

  **Pattern References**:
  - `internal/controller/postgresuser_controller_test.go:709` — `rp.keepSecretName = false` to remove
  - `internal/controller/postgresuser_controller_test.go:744` — assertion to update from `"mysecret-myuser"` to `"mysecret"`
  - `internal/controller/postgresuser_controller_test.go:758` — `rp.keepSecretName = false` to remove
  - `internal/controller/postgresuser_controller_test.go:788` — assertion to update from `"mysecret2-myuser2"` to `"mysecret2"`
  - `internal/controller/postgresuser_controller_test.go:792-820` — entire test case to DELETE
  - `internal/controller/postgresuser_controller_test.go:824` — `rp.keepSecretName = false` to remove

  **WHY Each Reference Matters**:
  - Field assignments (`rp.keepSecretName = false/true`) won't compile after field removal
  - Assertions with `{secretName}-{crName}` format need to become just `{secretName}`
  - The "keepSecretName=true" test case is entirely about removed behavior — delete it

  **Acceptance Criteria**:

  **QA Scenarios (MANDATORY):**

  ```
  Scenario: All unit tests pass after changes
    Tool: Bash
    Preconditions: Tasks 1, 2, 3 all applied
    Steps:
      1. Run: make test
      2. Assert: exit code 0, all tests pass
    Expected Result: PASS with 0 failures
    Failure Indicators: Any FAIL line, non-zero exit code
    Evidence: .sisyphus/evidence/task-3-unit-tests-pass.txt

  Scenario: No references to keepSecretName in test file
    Tool: Bash (grep)
    Preconditions: File edited
    Steps:
      1. Run: grep -n "keepSecretName" internal/controller/postgresuser_controller_test.go
      2. Assert: zero matches
    Expected Result: No output (zero matches)
    Failure Indicators: Any remaining reference
    Evidence: .sisyphus/evidence/task-3-tests-no-keepsecretnme.txt
  ```

  **Commit**: YES (groups with Tasks 1, 2, 4 in single commit)
  - Files: `internal/controller/postgresuser_controller_test.go`

- [x] 4. Update operator.yaml, e2e assertion, and README

  **What to do**:
  - In `config/manager/operator.yaml`: Remove lines 32-33 (the `KEEP_SECRET_NAME` env var entry)
  - In `tests/e2e/basic-operations/02-assert.yaml`: Change line 23 from `name: my-secret-my-db-user` to `name: my-secret`
  - In `README.md`:
    - Remove the `KEEP_SECRET_NAME` row from the Configuration table (line ~64)
    - Remove the Note/warning about `KEEP_SECRET_NAME` and secret name conflicts (lines ~68)
    - Update the PostgresUser section: change `my-secret-my-db-user` to `my-secret` and remove the parenthetical `(unless KEEP_SECRET_NAME is enabled)` (line ~174)

  **Must NOT do**:
  - Do NOT modify Helm chart files (no references exist there)
  - Do NOT change any CRD YAML files
  - Do NOT rewrite unrelated sections of README

  **Recommended Agent Profile**:
  - **Category**: `quick`
    - Reason: 3 files, simple text removals and updates
  - **Skills**: []

  **Parallelization**:
  - **Can Run In Parallel**: YES (independent of Go code changes)
  - **Parallel Group**: Wave 2 (can run parallel with Task 5 prep)
  - **Blocks**: Task 5
  - **Blocked By**: None (can start immediately, but logically grouped in Wave 2)

  **References**:

  **Pattern References**:
  - `config/manager/operator.yaml:32-33` — `KEEP_SECRET_NAME` env var entry to remove
  - `tests/e2e/basic-operations/02-assert.yaml:23` — `name: my-secret-my-db-user` → `name: my-secret`
  - `tests/e2e/basic-operations/02-postgresuser.yaml:8` — shows `secretName: my-secret` (this is correct, stays as-is)
  - `README.md:64` — `KEEP_SECRET_NAME` row in config table
  - `README.md:68` — Warning note about KEEP_SECRET_NAME
  - `README.md:174` — `my-secret-my-db-user (unless KEEP_SECRET_NAME is enabled)`

  **WHY Each Reference Matters**:
  - `operator.yaml` configures the deployment — stale env var is confusing
  - E2E assertion hardcodes old secret name format — will fail in CI if not updated
  - README documents removed behavior — stale docs mislead users

  **Acceptance Criteria**:

  **QA Scenarios (MANDATORY):**

  ```
  Scenario: No KEEP_SECRET_NAME references in YAML or docs
    Tool: Bash (grep)
    Preconditions: Files edited
    Steps:
      1. Run: grep -rn "KEEP_SECRET_NAME\|keepSecretName\|keep_secret_name" --include="*.yaml" --include="*.md" .
      2. Assert: zero matches
    Expected Result: No output (zero matches)
    Failure Indicators: Any remaining reference
    Evidence: .sisyphus/evidence/task-4-yaml-docs-clean.txt

  Scenario: E2E assertion uses correct secret name
    Tool: Bash (grep)
    Preconditions: File edited
    Steps:
      1. Run: grep "name:" tests/e2e/basic-operations/02-assert.yaml | grep -i secret
      2. Check that secret name is "my-secret" (not "my-secret-my-db-user")
    Expected Result: Line shows `name: my-secret`
    Failure Indicators: `my-secret-my-db-user` still present
    Evidence: .sisyphus/evidence/task-4-e2e-assert-name.txt
  ```

  **Commit**: YES (groups with Tasks 1, 2, 3 in single commit)
  - Files: `config/manager/operator.yaml`, `tests/e2e/basic-operations/02-assert.yaml`, `README.md`

---

## Final Verification Wave

- [ ] F1. **Zero References Check** — `quick`
  Run `grep -rn "keepSecretName\|KeepSecretName\|KEEP_SECRET_NAME\|keep_secret_name" --include="*.go" --include="*.yaml" --include="*.md" .`
  Expected: zero matches.
  Run `ast_grep_search` for `keepSecretName` in Go files as backup.
  Output: `References [0 found] | VERDICT: APPROVE/REJECT`

- [ ] F2. **Build + Test Verification** — `quick`
  Run `go build ./...` — expect clean compilation.
  Run `make test` — expect all tests pass, 0 failures.
  Run `lsp_diagnostics` on `pkg/config/config.go` and `internal/controller/postgresuser_controller.go` — expect 0 errors.
  Output: `Build [PASS/FAIL] | Tests [N pass/N fail] | VERDICT`

---

## Commit Strategy

**Single commit** — all changes are logically cohesive (removing one feature flag).

- **Message**: `refactor: remove KEEP_SECRET_NAME; always use spec.secretName as secret name`
- **Body**: `BREAKING CHANGE: Secret names now always equal spec.secretName from PostgresUser CR. The previous default behavior of appending "-{crName}" suffix is removed.`
- **Files**: `pkg/config/config.go`, `internal/controller/postgresuser_controller.go`, `internal/controller/postgresuser_controller_test.go`, `config/manager/operator.yaml`, `tests/e2e/basic-operations/02-assert.yaml`, `README.md`
- **Pre-commit**: `make test`

---

## Success Criteria

### Verification Commands
```bash
grep -rn "keepSecretName\|KeepSecretName\|KEEP_SECRET_NAME" .  # Expected: zero matches
go build ./...  # Expected: clean build
make test  # Expected: all tests pass
```

### Final Checklist
- [ ] All "Must Have" present
- [ ] All "Must NOT Have" absent
- [ ] All tests pass
- [ ] Zero stale references to KEEP_SECRET_NAME in repo
