# Aturan Modul Migration (Milestone 2)

## 1. Definisi, Immutability, & Urutan Linear

- Setiap migration memiliki nomor `version` linear per database dan `current_revision` yang bertambah setiap kali draft diubah sebelum pernah dieksekusi.
- Setiap migration wajib menyertakan `upSQL` dan salah satu dari:
  - `downSQL` (`reversible = true`), atau
  - penanda `reversible = false` beserta `irreversibleReason`.
- Setelah revisi migration pernah dieksekusi (`has_executed = true`), `upSQL` dan `downSQL` bersifat **immutable** dan tidak dapat ditimpa. Perbaikan wajib dibuat sebagai migration baru.
- **Rollback** hanya dapat dijalankan pada migration aktif terakhir (`A -> B -> C` wajib rollback `C` sebelum `B`) dan hanya jika `reversible = true`. Rollback yang gagal tidak mengubah posisi migration aktif.
- **Forward** menjalankan ulang `upSQL` immutable secara linear tanpa otomatis mengeksekusi migration berikutnya.

## 2. Workflow Review & Approval Multi-Grup

- Alur kerja: `Draft -> Pending Review -> Approved -> Queued -> Running -> Succeeded / Failed` (ditambah `partial`, `partially_reverted`, dan `uncertain` untuk kasus batch/gangguan).
- Setiap operasi yang mengubah database (`apply`, `rollback`, `forward`, `resume`, `cancel_batch`) wajib melewati review request dan mendapat persetujuan dari **active leader setiap grup yang memiliki akses ke database tersebut**, baik `view_only` maupun `view_edit`, menggunakan `accesscontrol.Store.ListDatabaseApprovers`.
- Jika ada grup akses yang belum memiliki leader aktif, pengajuan review ditolak (`409 Conflict`).
- Jika daftar grup akses, leader grup, atau revisi migration berubah sebelum eksekusi, review request lama otomatis di-`invalidated` dan harus diajukan ulang.
- Seluruh event pengajuan, keputusan approve/reject, dan invalidasi disimpan secara append-only di `mg_review_audit_events`.

## 3. Asynchronous Queue, Worker, Concurrency, & Pemulihan Lock

- Seluruh eksekusi migration (standar, batch, rollback, forward, resume, cancel_batch) dipublikasikan sebagai job persisten di tabel `mg_jobs` (`202 Accepted` dengan `jobId`) dan dijalankan secara asynchronous oleh worker Go (`cmd/worker` atau embedded worker di `cmd/api`).
- **Per-database distributed lock (`mg_db_locks`):**
  - Maksimal satu migration berjalan pada satu target database pada waktu yang sama; database berbeda dapat diproses secara paralel oleh worker.
  - Lock memiliki `lease_until` (default 30 detik) dan diperbarui berkala melalui heartbeat goroutine setiap `leaseTTL / 3` sehingga lock tetap aktif lintas commit batch.
  - **Pemulihan lock saat worker gagal (crash recovery):** Jika worker berhenti mendadak, heartbeat berhenti dan `lease_until` kedaluwarsa. Sweep `RecoverExpiredLocks` mendeteksi lock kedaluwarsa:
    - Untuk job **batch** yang sudah memiliki batch committed (`committed_batches > 0`), status job dipindahkan ke `partial` agar dapat dilanjutkan dari checkpoint (`resume`) atau dikompensasi (`cancel_batch`), lalu lock dilepas.
    - Untuk job **standard** (atau batch sebelum checkpoint pertama) yang hasil commit-nya belum pasti, status dipindahkan ke `uncertain` dan migration berikutnya pada database tersebut tetap ditahan sampai operator memverifikasi target database dan meresolusi statusnya.
- **Idempotency & Uncertain Commit:**
  - Tabel `_mg_execution_markers` (mode standar) dan `_mg_batch_checkpoints` (mode batch) pada target database dicatat **dalam transaksi yang sama** dengan perubahan data.
  - Sebelum retry atau menerima pesan duplikat, worker memeriksa tabel bukti eksekusi tersebut untuk mencegah double commit.
  - Kegagalan sementara (SQLSTATE `40001`, `40P01`, `55P03`, `57P03`, `53300`, `08*`, atau timeout koneksi) di-retry dengan exponential backoff terbatas (`max_attempts = 3`).

## 4. Mode Batch, Resume, & Kompensasi Terbalik

- Mode `batch` memproses data bertahap menggunakan cursor keyset (`WHERE <key> > $1 AND <key> <= $2`) dengan `batchSize` yang dapat dikonfigurasi tanpa memuat seluruh dataset ke memory.
- Setiap batch dieksekusi pada transaksi tersendiri bersama penyimpanan checkpoint di `_mg_batch_checkpoints`. Jika batch ke-`K` gagal, hanya batch ke-`K` yang di-rollback; batch `1..K-1` tetap committed dan job berstatus `partial`.
- **Resume (`resume`)** melanjutkan dari checkpoint terakhir tanpa mengulang batch yang sudah committed.
- **Pembatalan batch (`cancel_batch`)** menjalankan kompensasi per batch menggunakan `downSQL` immutable dalam urutan terbalik (`K-1 .. 1`). Jika gagal sebagian, progres disimpan dan status menjadi `partially_reverted`. Jika migration irreversible, pembatalan ditolak.

## 5. Riwayat Eksekusi Append-Only & Hide Rollback

- Seluruh attempt eksekusi (**berhasil maupun gagal**, termasuk rollback, forward, retry, resume, dan cancel_batch) dicatat secara append-only di tabel `mg_executions` beserta salinan query SQL yang dijalankan, `executionId`, `migrationId`, `revision`, `actor`, waktu, status, dan `targetExecutionId` (untuk rollback).
- Filter `GET /api/databases/{databaseID}/migrations?hideRollback=true` hanya menyembunyikan pasangan `(eksekusi yang dibatalkan, rollback sukses yang membatalkannya)` berdasarkan `executionId` pada respons API tanpa menghapus data riwayat maupun audit log. Attempt gagal dan forward berikutnya tetap tampil.

## 6. Activity Logging & OpenTelemetry Tracing

- Setiap aksi API, enqueue, klaim worker, lock, revalidasi, eksekusi SQL, commit, dan rollback dicatat sebagai structured JSON log melalui `log/slog` ke `stderr` dan dihubungkan lewat OpenTelemetry trace context (`traceparent` dipropagasikan melalui `mg_jobs.trace_context`) dengan `trace_id`, `span_id`, dan `correlation_id`.
