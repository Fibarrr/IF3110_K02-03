# IF3110 Milestone 1

Backend Go dan PostgreSQL lokal untuk platform pengelolaan database.

## Menjalankan backend

Butuh Go 1.22+ dan Docker Desktop. Salin `.env.example` jika belum punya `.env`, lalu jalankan PostgreSQL dari root repo:

```powershell
Copy-Item .env.example .env
docker compose up -d postgres
```

Compose membaca port dan kredensial database dari `.env`. Kalau `POSTGRES_PORT` diubah (misalnya ke `5433` karena `5432` sedang dipakai), pakai port yang sama di `DATABASE_URL`. Go tidak membaca `.env` otomatis. Di PowerShell yang sama, isi konfigurasi backend seperti contoh berikut; sesuaikan `5432` dengan port lokalmu dan ganti password admin dengan password rahasia minimal 12 karakter:

```powershell
$env:DATABASE_URL = "postgres://platform:platform_dev_password@127.0.0.1:5432/platform_dev?sslmode=disable"
$env:PLATFORM_ADMIN_USERNAME = "admin"
$env:PLATFORM_ADMIN_PASSWORD = "ganti-dengan-password-rahasia"
$env:BACKEND_PORT = "8080"
Set-Location backend
go mod tidy
go run ./cmd/api
```

Saat pertama kali jalan, backend membuat tabel Access Control dan akun Platform Admin jika belum ada admin aktif. Backend hanya memakai environment variable dari prosesnya; file `.env` dibaca otomatis oleh Docker Compose, bukan oleh Go.

Health check: `GET http://localhost:8080/health`.

## Menjalankan frontend

Frontend memakai Go templates dan file CSS/JavaScript yang disajikan langsung oleh backend. Tidak perlu menjalankan server frontend terpisah atau memasang dependency Node.js.

1. Pastikan PostgreSQL dari Docker Compose berjalan.
2. Jalankan backend mengikuti bagian **Menjalankan backend** dan biarkan prosesnya tetap hidup.
3. Buka `http://localhost:8080/login` di browser. Setelah login, aplikasi akan membuka halaman daftar database.

Halaman admin tersedia di `http://localhost:8080/admin` dan hanya dapat dibuka oleh Platform Admin. Pengguna lain akan tetap di halaman tersebut dan melihat pesan akses khusus admin.

## Automated testing

Jalankan seluruh unit test dari folder `backend`:

```powershell
go test ./...
```

Untuk menjalankan juga tes integrasi PostgreSQL, pastikan PostgreSQL dari Compose sedang berjalan. Dari folder `backend`, isi `TEST_DATABASE_URL` dengan kredensial dan port yang sama seperti `.env`:

```powershell
$env:TEST_DATABASE_URL = "postgres://platform:platform_dev_password@127.0.0.1:5432/platform_dev?sslmode=disable"
go test ./... -run 'TestPostgres' -v
```

Ganti port `5432` jika `POSTGRES_PORT` di `.env` berbeda. Tes integrasi membuat schema sementara (`ac_test_...`, `dm_test_...`, dan `mg_test_...`) lalu menghapusnya setelah selesai. Gunakan database development lokal. Pesan `no tests to run` pada paket tanpa test bernama `TestPostgres` adalah normal; jalankan `go test ./...` untuk seluruh test.

## Testing Flow

Gunakan database target sementara yang berisi tabel dan data contoh. Jangan jalankan perubahan migration pada database yang menyimpan data penting.

1. Login sebagai Platform Admin, daftarkan koneksi PostgreSQL target, lalu gunakan **Uji Koneksi**.
2. Buat pengguna biasa dan grup. Tambahkan pengguna sebagai anggota grup, lalu berikan akses **View Only** ke database target.
3. Login sebagai pengguna biasa. Buka database dan periksa schema, tabel, kolom, constraint, index, serta data. Schema Visualizer dapat digunakan untuk melihat relasi Foreign Key.
4. Buka `/admin` sebagai pengguna biasa dan pastikan halaman menampilkan peringatan bahwa akses hanya untuk Platform Admin. Di Migration Editor, akses **View Only** hanya dapat melihat riwayat dan detail migration.
5. Sebagai admin, ubah akses grup menjadi **View + Edit**. Muat ulang halaman database pengguna biasa, buat draft migration, lalu simpan dan jalankan. Contoh perubahan sederhana:

   ```sql
   ALTER TABLE public.customers ADD COLUMN test_note TEXT;
   ```

   Periksa bahwa kolom muncul di viewer dan migration tercatat sukses di riwayat.

6. Uji rollback dengan migration yang melakukan perubahan lalu gagal:

   ```sql
   CREATE TABLE public.rollback_probe (id INT PRIMARY KEY);
   SELECT no_such_migration_function();
   ```

   Pastikan migration berstatus gagal dan tabel `rollback_probe` tidak terbentuk. Migration gagal menahan versi berikutnya; perbaiki draft yang sama, misalnya menjadi `SELECT 1;`, simpan, lalu jalankan agar urutan migration tidak tertahan.
7. Sebagai admin, cabut akses grup atau keluarkan pengguna dari grup. Muat ulang halaman sebagai pengguna tersebut dan pastikan ia tidak dapat membuka database lagi.

## API Access Control

- `POST /api/auth/login`
- `POST /api/auth/logout`
- `GET /api/auth/me`
- `GET /api/admin/users`, `POST /api/admin/users`
- `PATCH /api/admin/users/{userID}/password`
- `PATCH /api/admin/users/{userID}/disabled`
- `PATCH /api/admin/users/{userID}/platform-admin`
- `GET /api/admin/groups`, `POST /api/admin/groups`
- `DELETE /api/admin/groups/{groupID}`
- `GET /api/admin/groups/{groupID}/members`
- `GET /api/admin/groups/{groupID}/databases`
- `PUT` atau `DELETE /api/admin/groups/{groupID}/members/{userID}`
- `PUT /api/admin/groups/{groupID}/databases/{databaseID}/access`
- `DELETE /api/admin/groups/{groupID}/databases/{databaseID}/access`
- `GET /api/auth/databases/{databaseID}/access`

Login memakai username dan password. Password di-hash dengan bcrypt; panjangnya 12–72 byte. Sesi disimpan di PostgreSQL sebagai hash ID sesi, dan ID aslinya hanya dikirim lewat cookie HttpOnly. Endpoint yang mengubah data butuh cookie CSRF dan header `X-CSRF-Token` dari respons login atau `/api/auth/me`.

Izin database diberikan lewat grup. Kirim nilai `view_only` atau `view_edit` saat mengatur izin. Jika pengguna mendapat izin dari beberapa grup, `view_edit` mengungguli `view_only`. Platform Admin dapat mengelola platform, tetapi tidak otomatis punya izin pada database tertentu. Modul Database Management dapat memakai `Store.EffectiveAccess` atau membungkus route dengan `HTTPHandler.RequireDatabaseAccess`; `UserFromContext` mengambil user yang sudah login.

Jangan kirim ID sesi lewat URL atau simpan di local storage.

Nilai sesi saat ini: idle timeout 30 menit, masa berlaku maksimum 12 jam, rotasi setiap 15 menit, dan grace period token lama 30 detik. Rotasi tidak memperpanjang masa berlaku maksimum.

## Database Management

Halaman database menyediakan **Schema Visualizer** untuk melihat tabel dan relasi Foreign Key, **Schema & Table Viewer** untuk melihat kolom, constraint, index, dan data tabel secara read-only, serta **Migration Editor** untuk mengelola draft dan menjalankan migration.

## Migration

Migration disimpan sebagai draft dan riwayat di database platform, lalu SQL dijalankan ke database target. Migration diberi versi berurutan per database; versi berikutnya menunggu versi sebelumnya sukses. Eksekusi memakai transaksi PostgreSQL, maksimal 256 KiB per SQL dan 2 menit per eksekusi. UI Migration tersedia pada tab **Migration Editor** di `/databases/{databaseID}`. Lihat aturan dan format API di `backend/internal/migration/POLICY.md`.

- `GET /api/databases/{databaseID}/migrations` dan `GET /api/databases/{databaseID}/migrations/{migrationID}` untuk melihat SQL dan riwayat (perlu `view_only`).
- `POST /api/databases/{databaseID}/migrations` untuk membuat draft.
- `PUT /api/databases/{databaseID}/migrations/{migrationID}` untuk mengubah draft atau migration yang gagal.
- `POST /api/databases/{databaseID}/migrations/{migrationID}/execute` untuk menjalankan SQL (perlu `view_edit`).

Semua request yang mengubah data juga perlu token CSRF. Jalankan tes unit Migration dengan `go test ./internal/migration`; tes integrasi PostgreSQL dapat dijalankan dengan `TEST_DATABASE_URL` yang sama seperti bagian tes integrasi di atas.
