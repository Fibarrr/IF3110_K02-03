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

- `GET /api/databases/{databaseID}/migrations`
- `GET /api/databases/{databaseID}/migrations/{migrationID}`
- `POST /api/databases/{databaseID}/migrations`
- `PUT /api/databases/{databaseID}/migrations/{migrationID}`

The Milestone 2 definition needs a name, `upSQL`, and either `downSQL` or an `irreversibleReason`. It also needs an execution mode (`standard` or `batch`) and batch settings for a batch migration. The exact batch payload should be agreed with the worker implementation; do not imply that arbitrary SQL can be automatically split into safe batches.

Editing SQL creates/increments a revision. A review or execution must refer to the exact revision it uses. Editing a revision invalidates approvals for the earlier revision. Once a revision has executed, its SQL is immutable; corrections use a new migration.

### Submit a change for review

Proposed route:

`POST /api/databases/{databaseID}/migrations/{migrationID}/review-requests`

Example request for the first application:

```json
{ "operation": "apply", "revision": 2 }
```

`operation` is `apply`, `rollback`, or `forward`. Rollback/forward requests must identify the execution they target, for example with `targetExecutionId`. Every operation that changes the database needs its own review request and approval set.

Response: `201 Created` with a request ID, operation, referenced revision/execution, status (`pending_review`), and the required groups/approval decisions. The server derives required groups from `ListDatabaseApprovers`; the client must not choose the approver set.

### Approve or reject

Proposed route:

`PUT /api/databases/{databaseID}/review-requests/{requestID}/approvals/{groupID}`

Request:

```json
{ "decision": "approve", "comment": "Sudah direview" }
```

Only the active leader of that required group may decide. The server also verifies current database access, session, and CSRF. A decision for a group that does not have access to this database is rejected. Store an audit event when a decision changes; do not erase prior decisions from the audit record.

The request is `approved` only when every required group has approved it. Any rejection prevents queueing. The response includes the latest decision per group and the request's current status.

### Queue and monitor execution

Proposed routes:

- `POST /api/databases/{databaseID}/review-requests/{requestID}/queue`
- `GET /api/databases/{databaseID}/jobs/{jobID}`
- `POST /api/databases/{databaseID}/jobs/{jobID}/resume`

Queueing is allowed only after approval and server-side revalidation. Return `202 Accepted` with a job ID immediately; the API must not wait for SQL execution. Job status should expose at least `queued`, `running`, `succeeded`, `failed`, and partial/partially-reverted states when batching applies. Include committed batch/row counts when available and a safe, useful error summary on failure.

The worker must revalidate the referenced revision, approval quorum, caller authorization, and migration order before execution. If the approver set or its leader changes after review, invalidate the old approval set and require a new review. Jobs/checkpoints must survive process restart; duplicate queue delivery must not double-commit a batch.

### Migration history

`GET /api/databases/{databaseID}/migrations?hideRollback=true|false`

The filter only changes the response; it must not delete or rewrite history. Keep execution IDs so a rollback identifies the execution it reverses. Failed attempts remain visible in application logs/audit records; successful committed changes remain in migration history. If the team interprets the PDF's history wording differently, resolve it against the latest PDF/Q&A and update this contract before implementing the response.

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
