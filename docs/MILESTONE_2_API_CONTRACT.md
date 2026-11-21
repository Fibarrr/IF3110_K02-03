# Milestone 2 API Contract (Draft for Team Coordination)

**Audience:** all team members and coding agents working on Milestone 2. Read this before changing an API used by another module.

**Status:** shared working draft. The Milestone 2 PDF remains the source of requirements. The paths and payloads below are a proposed agreement to let frontend, Access Control, and Migration work in parallel. If an endpoint or payload does not fit an implementation, change this document and notify every affected owner before merging the code. Do not silently change an agreed contract.

## Shared conventions

- JSON fields use `camelCase`; IDs use the existing integer IDs unless an existing module already uses a string ID (database IDs are strings).
- Keep the Milestone 1 login, session cookie, CSRF, database access, and admin APIs working. Mutating requests continue to send `X-CSRF-Token` and the existing CSRF cookie.
- Use the existing JSON error shape: `{"error":"..."}`. Use `400` for invalid input, `401` for missing/invalid session, `403` for insufficient permission, `404` for missing resources, `409` for state conflicts, and `5xx` for server/dependency errors.
- Check database permission on the server for every request. Hiding a button in the SPA is not authorization.
- Keep module ownership clear: Access Control owns `ac_*`; Migration owns `mg_*`. Migration should call an Access Control service/interface for group leaders and grants instead of querying or modifying `ac_*` tables directly.

## Access Control — implemented foundation

### Read groups and their leaders

`GET /api/admin/groups`

The existing response remains `{ "groups": [...] }`. Each group now includes:

```json
{
  "id": 4,
  "name": "backend",
  "leaderUserId": 12,
  "leaderUsername": "rafi",
  "createdAt": "2026-11-19T12:00:00Z"
}
```

`leaderUserId` is `null` when no valid active member is assigned. The admin UI should make that state visible and provide a way to assign a leader.

### Assign or replace a group leader

`PUT /api/admin/groups/{groupID}/leader`

Request:

```json
{ "userId": 12 }
```

Success: `204 No Content`. The caller must be a logged-in Platform Admin and pass CSRF validation. The selected user must be an active member of that group. A leader cannot be removed from the group or disabled until another leader is assigned.

### Resolve approval groups for a database

Access Control exposes this Go service method for Migration:

```go
ListDatabaseApprovers(ctx context.Context, databaseID string) ([]DatabaseApprover, error)
```

It returns one row for every group with a grant on the database, whether the grant is `view_only` or `view_edit`. Each row has `GroupID`, `GroupName`, `Access`, `LeaderUserID`, and `LeaderUsername`. A nil `LeaderUserID` means that group has no valid leader, so Migration must not proceed with review/execution until one is assigned.

## Migration — proposed workflow API

The current Milestone 1 routes are draft/history CRUD plus synchronous execution. Milestone 2 needs review requests and asynchronous jobs. The Migration owner may adapt route names to fit the existing store, but preserve the behavior and response information below, then update this document before frontend integration.

### Migration definition

Keep the existing routes for listing, viewing, creating, and editing drafts:

- `GET /api/databases/{databaseID}/approvers`
- `GET /api/databases/{databaseID}/migrations?hideRollback=true|false`
- `GET /api/databases/{databaseID}/migrations/{migrationID}`
- `POST /api/databases/{databaseID}/migrations`
- `PUT /api/databases/{databaseID}/migrations/{migrationID}`

The Milestone 2 definition accepts:

```json
{
  "name": "Backfill orders status",
  "upSQL": "UPDATE public.orders SET status = 'v2' WHERE id > $1 AND id <= $2",
  "downSQL": "UPDATE public.orders SET status = 'v1' WHERE id > $1 AND id <= $2",
  "reversible": true,
  "irreversibleReason": "",
  "executionMode": "batch",
  "batchConfig": {
    "tableSchema": "public",
    "tableName": "orders",
    "keyColumn": "id",
    "batchSize": 5000
  }
}
```

For `executionMode: "standard"`, `batchConfig` is omitted. When `reversible` is `false`, `irreversibleReason` is required and `downSQL` is empty. For `executionMode: "batch"`, `upSQL` (and `downSQL` when reversible) must scope each batch with keyset cursor parameters `$1` (exclusive lower bound `last_id`) and `$2` (inclusive upper bound `end_id`).

Editing SQL creates/increments a revision. A review or execution must refer to the exact revision it uses. Editing a revision invalidates approvals for the earlier revision. Once a revision has been executed, its SQL is immutable; corrections use a new migration.

### Submit a change for review

Implemented routes:

- `POST /api/databases/{databaseID}/migrations/{migrationID}/review-requests`
- `GET /api/databases/{databaseID}/review-requests`
- `GET /api/databases/{databaseID}/review-requests/{requestID}`

Example request:

```json
{ "operation": "apply", "revision": 1, "comment": "Siap ditinjau" }
```

`operation` is `apply`, `rollback`, `forward`, `resume`, or `cancel_batch`. Rollback requests reference `targetExecutionId` (optional if targeting the active execution); `resume` and `cancel_batch` reference `targetJobId` (optional if targeting the latest partial job). Every operation that changes the database needs its own review request and approval set.

Response: `201 Created` with request ID, operation, referenced revision/execution/job, status (`pending_review`), and the required groups/approval decisions derived server-side from `ListDatabaseApprovers`.

### Approve or reject

Implemented route:

`PUT /api/databases/{databaseID}/review-requests/{requestID}/approvals/{groupID}`

Request:

```json
{ "decision": "approve", "comment": "Sudah direview" }
```

Only the active leader of that required group may decide (including leaders of `view_only` groups). The server verifies current database access, session, CSRF, and live approver snapshot consistency. Every decision is appended to `mg_review_audit_events`.

### Queue and monitor execution

Implemented routes:

- `POST /api/databases/{databaseID}/review-requests/{requestID}/queue` (`202 Accepted`)
- `GET /api/databases/{databaseID}/jobs`
- `GET /api/databases/{databaseID}/jobs/{jobID}`
- `POST /api/databases/{databaseID}/jobs/{jobID}/resume` (`202 Accepted`, requires approved `resume` review request)
- `POST /api/databases/{databaseID}/jobs/{jobID}/resolve-uncertain` (resolves an `uncertain` job after operator DB verification)

Queueing is allowed only after approval and server-side revalidation. Returns `202 Accepted` with the job immediately without waiting for SQL execution. Job statuses include `queued`, `running`, `succeeded`, `failed`, `partial`, `partially_reverted`, and `uncertain`, with `committedBatches`, `committedRows`, `lastCursor`, `maxCursor`, and a safe `errorSummary`.

### Migration history

`GET /api/databases/{databaseID}/migrations?hideRollback=true|false`

Returns `{ "migrations": [...], "history": [...], "hideRollback": false }`.

Per Milestone 2 Specification v4 (revised 16 Nov 2026), **all executed attempts (both succeeded and failed)** are persisted append-only in `mg_executions` with `executionId`, `migrationId`, `revision`, `operation`, `targetExecutionId`, `executedBy`, `sqlExecuted`, `status`, timestamps, and batch progress counters. When `hideRollback=true`, the API hides only `(cancelled execution, successful rollback/cancel_batch)` pairs matched by `executionId`; later `forward` executions and failed attempts remain visible, and no history or audit records are deleted.

## Required behavior to preserve across modules

- Every group with any access to the database participates in approval, including `view_only` groups.
- The submitter must have `view_edit` permission. A Platform Admin is not automatically a database approver or database editor.
- Rollback is limited to the active last execution when reversible. Forward re-applies the original immutable `upSQL`; it does not automatically run later migrations.
- All execution types, including rollback/forward and batch jobs, use the asynchronous queue/worker.
- Use structured JSON logs for attempts and failures, and propagate OpenTelemetry trace context through API, queue, worker, lock, and SQL work. Redact credentials, session identifiers, and sensitive row values.
- Each backend feature needs unit and integration coverage for success, failure, and denied access. Batch testing must use the same large dataset for the unbatched baseline and batched run, and record row count and duration.

## Coordination notes for contributors

1. Access Control owns leader assignment and `ListDatabaseApprovers`; Migration owns review requests, decisions, queue/jobs, and execution history.
2. Frontend consumes the routes above and displays API state. It must not calculate approval quorum or treat hidden buttons as security controls.
3. If you need a route/payload change, update this file in the same change and tell the owners of the affected module before merging. Keep old Milestone 1 behavior working unless the new requirement explicitly replaces it.
4. Do required features first. Centralized observability, Prometheus/Grafana dashboards, k6 stress testing, and CodeQL are bonuses; OpenTelemetry tracing and structured application logs are required.
