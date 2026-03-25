# Failing E2E Test Suites

> **Last updated**: 2026-03-25, branch `fix/operator-bug-fixes`
> **CI run**: [#23528119620](https://github.com/vterdunov/external-postgres-control-operator/actions/runs/23528119620)
> **Status**: 6 of 13 suites fail; all failures are on delete steps (user roles not being dropped)

## Quick Summary

All 6 failing tests share the **same root cause**: when a `Postgres` CR and its associated `PostgresUser` CRs are deleted concurrently, the operator fails to drop the PostgreSQL user roles. Databases ARE dropped successfully — only user role cleanup fails.

The 7 passing suites are enabled in `.github/workflows/e2e-test.yml` via `--test` flags.

---

## Failing Test Suites

### 1. `annotations-labels` — Step `3-delete`

- **Error**: `User role(s) still exist but should be dropped`
- **What it does**: Creates a Postgres CR + PostgresUser with custom annotations/labels, then deletes both and asserts roles are gone.

### 2. `minimal-cr` — Step `3-delete`

- **Error**: `User role(s) still exist but should be dropped`
- **What it does**: Creates a minimal Postgres CR + PostgresUser, then deletes both and asserts roles are gone.

### 3. `multiple-users` — Step `3-delete-users`

- **Error**: `Role with prefix mowner still exists`
- **What it does**: Creates a Postgres CR + multiple PostgresUser CRs (owner, reader, writer), deletes the users, and asserts all roles are gone.

### 4. `spec-updates` — Step `6-delete`

- **Error**: `Role(s) with prefix upduser still exist`
- **What it does**: Creates resources, updates specs in several steps, then deletes and asserts roles are gone.

### 5. `idempotency` — Step `4-delete`

- **Error**: `User role idemuser* still exists after deletion`
- **What it does**: Creates a Postgres CR + PostgresUser, reconciles multiple times, then deletes both and asserts roles are gone.

### 6. `cleanup-verification` — Step `3-delete-users`

- **Error**: `cowner roles still exist but should be dropped`
- **What it does**: Creates a Postgres CR + PostgresUser CRs, deletes the user CRs, and asserts roles are gone.

---

## Root Cause Analysis

### The Race Condition: `addOwnerRef` + Cascade Deletion

**Core problem**: `PostgresUser` CRs have an `ownerReference` pointing to their parent `Postgres` CR (set by `addOwnerRef` at `postgresuser_controller.go:417-431`). When both CRs are deleted:

1. Test issues `kubectl delete` for both `Postgres` and `PostgresUser` CRs
2. `Postgres` CR finalizer runs → drops database + group roles ✅
3. `Postgres` CR is fully deleted
4. Kubernetes garbage collector sees `PostgresUser` owned by the now-deleted `Postgres` → **cascade-deletes** the `PostgresUser` CR
5. The `PostgresUser` finalizer either:
   - Never runs (CR is already gone), OR
   - Calls `getPostgresCR()` which fails (see below), causing a requeue loop until the CR is GC'd

### The `getPostgresCR` Blocker

**File**: `internal/controller/postgresuser_controller.go`, lines 320-336

```go
func (r *PostgresUserReconciler) getPostgresCR(...) (*dbv1alpha1.Postgres, error) {
    // ...
    if !database.Status.Succeeded {
        err = fmt.Errorf("database \"%s\" is not ready", database.Name)
        return nil, err  // NOT a NotFound error!
    }
    return &database, nil
}
```

When the `Postgres` CR is being deleted, its `Status.Succeeded` gets set to `false` by the `requeue()` function (`postgres_controller.go:133-141`). The `getPostgresCR` then returns a **non-nil, non-NotFound error**.

In the deletion logic (`postgresuser_controller.go:88-117`):

```go
if instance.GetDeletionTimestamp() != nil {
    // ...
    postgres, err := r.getPostgresCR(ctx, instance)
    if err != nil && !errors.IsNotFound(err) {
        return ctrl.Result{}, err  // ← REQUEUES instead of falling through to DropRole
    }
    // ...
}
```

The `"database X is not ready"` error is NOT a NotFound error, so it triggers a requeue. By the time the next reconcile happens, the Postgres CR may be gone (NotFound), but the PostgresUser CR may ALSO be gone (cascade-deleted by K8s GC).

### Why `basic-operations` Passes

`basic-operations` works because it deletes the `PostgresUser` **first**, waits for the role to be cleaned up, and **then** deletes the `Postgres` CR. This avoids the race condition entirely.

---

## Recommended Fixes (Choose One)

### Option A: Fix `getPostgresCR` During Deletion (Recommended)

In `postgresuser_controller.go:320-336`, when called from deletion context, skip the `!database.Status.Succeeded` check:

```go
// During deletion, we don't care if the database is "ready" —
// we just need the connection info to drop the role.
if !database.Status.Succeeded && instance.GetDeletionTimestamp().IsZero() {
    err = fmt.Errorf("database \"%s\" is not ready", database.Name)
    return nil, err
}
```

Or pass a `forDeletion bool` parameter and skip the check when true.

### Option B: Treat "not ready" as NotFound in Deletion

In `postgresuser_controller.go:97`, also handle the "not ready" case:

```go
if err != nil && !errors.IsNotFound(err) && !strings.Contains(err.Error(), "is not ready") {
    return ctrl.Result{}, err
}
```

This is more of a patch — Option A is cleaner.

### Option C: Remove `addOwnerRef` Entirely

Remove the `addOwnerRef` call (`postgresuser_controller.go:417-431`) so PostgresUser CRs are NOT owned by Postgres CRs. This prevents cascade deletion, giving the PostgresUser finalizer time to run.

**Trade-off**: Orphaned PostgresUser CRs would remain if a Postgres CR is deleted without deleting its users first. Whether this is acceptable depends on the desired behavior.

### Option D: Delete Users Before Database in Tests

Change all failing test delete steps to delete PostgresUser CRs first, wait for role cleanup, then delete the Postgres CR. This is a **test-only workaround** that doesn't fix the underlying operator bug.

---

## Key Files

| File | Relevant Lines | Purpose |
|------|---------------|---------|
| `internal/controller/postgresuser_controller.go` | 88-117 | Deletion logic / finalizer |
| `internal/controller/postgresuser_controller.go` | 320-336 | `getPostgresCR` — the blocker |
| `internal/controller/postgresuser_controller.go` | 417-431 | `addOwnerRef` — sets ownerReference |
| `internal/controller/postgresuser_controller.go` | 433-439 | `requeue` — sets `Succeeded=false` |
| `internal/controller/postgres_controller.go` | 84-130 | Postgres deletion logic |
| `internal/controller/postgres_controller.go` | 133-141 | `requeue` — sets `Succeeded=false` |
| `pkg/postgres/role.go` | `DropRole` | Role drop logic |
| `pkg/postgres/database.go` | `DropDatabase` | Database drop logic |

## Already-Fixed Bugs (for context)

These were fixed in earlier commits on this branch:

1. **`minimal-cr/01-assert.yaml`** expected `schemas: []` but the field is `omitempty` → removed from assert
2. **`Status.Succeeded` guard on deletion** in both controllers blocked finalizer cleanup → removed
3. **Unsafe `err.(*pq.Error)` type assertions** in `role.go` and `database.go` → converted to safe `, ok` pattern
4. **psql commands** missing `PGPASSWORD` and `-h 127.0.0.1` → fixed across all test files
5. **Cleanup timeouts** too short (60s) → increased to 120s

## Passing Test Suites (7/13)

- `basic-operations` ✅
- `drop-on-delete-false` ✅
- `extensions` ✅
- `privileges-read` ✅
- `privileges-write` ✅
- `secret-content` ✅
- `secret-template` ✅
