# IF3110 Milestone 2 — Database Management Platform

Backend Go, Asynchronous Migration Worker, PostgreSQL, dan Frontend Single-Page Application (SPA) responsif untuk pengelolaan akses database, eksplorasi schema/data, serta alur migration berbasis review, approval leader grup, queue, dan batch processing.

## Menjalankan Backend & Worker

Butuh Go 1.22+, Docker Desktop (untuk PostgreSQL lokal), dan Node.js 20+ (opsional jika ingin membangun ulang frontend SPA).

1. Salin `.env.example` menjadi `.env` (jika belum ada), lalu jalankan PostgreSQL:

```powershell
Copy-Item .env.example .env
docker compose up -d postgres
```

2. Isi `PLATFORM_ADMIN_USERNAME` dan `PLATFORM_ADMIN_PASSWORD` (minimal 12 karakter) di `.env`, lalu jalankan API server:

```powershell
Set-Location backend
go mod tidy
go run ./cmd/api
```

Secara default, `cmd/api` juga menjalankan **embedded worker** (`EMBEDDED_WORKER=true`) agar pengembangan lokal cukup memakai satu perintah. Untuk menjalankan proses worker terpisah (termasuk beberapa instance worker paralel):

```powershell
# Terminal 1: API tanpa embedded worker
$env:EMBEDDED_WORKER = "false"
go run ./cmd/api

# Terminal 2: Standalone Migration Worker
$env:WORKER_ID = "worker-a"
$env:WORKER_CONCURRENCY = "2"
go run ./cmd/worker
```

Health check: `GET http://localhost:8080/health`.

## Menjalankan & Membangun Frontend SPA

Seluruh halaman (Login, Daftar Database, Schema & Table Viewer, Schema Visualizer, Migration & Approval Workflow, Monitor Job Async, dan Panel Admin termasuk penetapan Leader Grup) telah diimplementasikan sebagai **Single-Page Application (SPA)** menggunakan React + Vite di folder `frontend/` dan responsif pada layar desktop maupun mobile.

- **Mode terintegrasi (disarankan):** Hasil build SPA berada di `backend/internal/web/dist` dan di-embed langsung ke binary Go, sehingga cukup jalankan `go run ./cmd/api` lalu buka `http://localhost:8080`.
- **Mode development (hot reload) atau build ulang:**

```powershell
Set-Location frontend
npm install
npm run dev      # dev server di http://localhost:5173 (proxy /api ke :8080)
npm run build    # membangun ulang ke backend/internal/web/dist
```

## Ringkasan Fitur Milestone 2

1. **Access Control & Group Leader (`AC2`):**
   - Setiap grup memiliki seorang leader aktif (`PUT /api/admin/groups/{groupID}/leader`).
   - Leader tidak dapat dikeluarkan dari grup atau dinonaktifkan sebelum diganti.
   - `ListDatabaseApprovers` mengembalikan seluruh grup dengan grant pada suatu database (baik `view_only` maupun `view_edit`).
2. **Migration Workflow, Approval, Rollback/Forward & Append-Only History (`MG2`):**
   - Definisi migration mendukung `upSQL` dan `downSQL` (`reversible: true`) atau penanda `irreversible` beserta alasannya, serta mode `standard` dan `batch`.
   - Setelah pernah dieksekusi, SQL revisi bersifat immutable; perbaikan wajib dibuat melalui migration baru.
   - Setiap operasi perubahan (`apply`, `rollback`, `forward`, `resume`, `cancel_batch`) wajib melalui review request dan persetujuan leader dari **semua grup** yang memiliki akses ke database (termasuk `view_only`).
   - Rollback hanya dapat dilakukan pada migration aktif terakhir yang reversible; forward menjalankan ulang `upSQL` asli secara linear tanpa otomatis menjalankan migration berikutnya.
   - Seluruh attempt eksekusi (**berhasil maupun gagal**) dicatat secara append-only pada tabel `mg_executions` beserta salinan SQL yang dijalankan. Checkbox **Hide rollback** (`?hideRollback=true`) menyembunyikan pasangan `(eksekusi yang dibatalkan, rollback sukses)` berdasarkan `executionId` tanpa menghapus riwayat.
3. **Asynchronous Queue, Per-Database Lock, Batching & Lock Recovery (`MG2`):**
   - Setiap eksekusi dipublikasikan ke queue persisten (`mg_jobs`) dengan respons `202 Accepted` dan diproses asynchronous oleh worker Go.
   - Maksimal satu migration aktif per database melalui distributed lease lock (`mg_db_locks`) dengan heartbeat otomatis; database berbeda diproses paralel.
   - **Pemulihan Lock saat Worker Gagal:** Jika worker berhenti mendadak, lease lock kedaluwarsa (`lease_until < NOW()`). Sweep `RecoverExpiredLocks` memulihkan lock kedaluwarsa: job batch yang telah memiliki checkpoint committed beralih ke `partial` (dapat dilanjutkan via `resume` atau dibatalkan per batch terbalik via `cancel_batch`), sedangkan job standar dengan status commit belum pasti beralih ke `uncertain` dan menahan migration berikutnya pada database tersebut hingga diverifikasi dan diresolusi.
   - Mode `batch` memproses data bertahap via cursor keyset (`WHERE <key> > $1 AND <key> <= $2`) dengan commit checkpoint atomik di target DB (`_mg_batch_checkpoints`) pada transaksi yang sama untuk mencegah double commit dan mendukung resume/kompensasi terbalik.
4. **Structured JSON Activity Logging & OpenTelemetry Tracing (`DM2` & `MG2`):**
   - Seluruh request HTTP, query read-only Database Management, enqueue, klaim worker, lock, revalidasi, dan eksekusi/commit/rollback SQL dicatat sebagai structured JSON log (`log/slog` ke `stderr`) dengan penyamaran kredensial, DSN password, token sesi, dan data sensitif.
   - OpenTelemetry tracing menghubungkan `API -> queue -> worker -> lock -> batch/DB query` dengan propagasi W3C `traceparent` di `mg_jobs.trace_context` serta `trace_id`/`span_id`/`correlation_id` pada log. Exporter console aktif secara default (`OTEL_EXPORTER_CONSOLE=true`).

### Contoh Output Structured JSON Log & OpenTelemetry Console Span

Contoh log JSON (`stderr`):

```json
{"time":"2026-11-21T14:20:01.102Z","level":"INFO","msg":"sql.start","service":"if3110-migration-worker","event":"sql.start","status":"started","correlation_id":"a9f1d02c8e1145ab","actor":{"user_id":4,"username":"editor","platform_admin":false},"database":"orders-db","migration_id":3,"job_id":12,"execution_id":18,"attempt":1,"batch":2,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7","query":"UPDATE public.orders SET status = 'v2' WHERE id > $1 AND id <= $2","params":["1000","2000"]}
{"time":"2026-11-21T14:20:01.148Z","level":"INFO","msg":"sql.result","service":"if3110-migration-worker","event":"sql.result","status":"ok","correlation_id":"a9f1d02c8e1145ab","database":"orders-db","migration_id":3,"job_id":12,"execution_id":18,"attempt":1,"batch":2,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7","duration_ms":46.2,"rows":1000}
{"time":"2026-11-21T14:20:01.151Z","level":"INFO","msg":"sql.commit","service":"if3110-migration-worker","event":"sql.commit","status":"committed","correlation_id":"a9f1d02c8e1145ab","database":"orders-db","migration_id":3,"job_id":12,"execution_id":18,"attempt":1,"batch":2,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7","duration_ms":2.9}
```

Contoh span OpenTelemetry console (`stdout`):

```json
{"Name":"worker.batch.tx","SpanContext":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"00f067aa0ba902b7"},"Parent":{"TraceID":"4bf92f3577b34da6a3ce929d0e0e4736","SpanID":"7b2101a490c11820"},"Attributes":[{"Key":"correlation.id","Value":{"Type":"STRING","Value":"a9f1d02c8e1145ab"}},{"Key":"db.id","Value":{"Type":"STRING","Value":"orders-db"}},{"Key":"batch.no","Value":{"Type":"INT64","Value":2}},{"Key":"batch.rows_affected","Value":{"Type":"INT64","Value":1000}}]}
```

## Automated Testing & Benchmark Dataset Besar

Jalankan seluruh unit test backend:

```powershell
Set-Location backend
go test ./... -v
```

Jalankan tes integrasi PostgreSQL (pastikan PostgreSQL berjalan dan `TEST_DATABASE_URL` terisi di `.env` atau shell):

```powershell
Set-Location backend
go test ./... -run 'TestPostgres' -v
```

Jalankan pengujian dataset besar (> 1 detik untuk `UPDATE` tanpa batch vs mode batch pada dataset yang sama):

```powershell
Set-Location backend
$env:RUN_LARGE_BATCH_BENCHMARK = "1"
go test ./internal/migration -run TestPostgresLargeDatasetBatchMigration -v
```

Lihat kontrak API lengkap di [docs/MILESTONE_2_API_CONTRACT.md](docs/MILESTONE_2_API_CONTRACT.md) serta detail kebijakan modul di `backend/internal/accesscontrol/POLICY.md`, `backend/internal/databasemanagement/POLICY.md`, dan `backend/internal/migration/POLICY.md`.
