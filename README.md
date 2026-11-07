# IF3110 Milestone 1

Aplikasi web untuk mengelola koneksi database PostgreSQL, melihat schema dan data, mengatur akses pengguna, serta menyusun dan menjalankan migration SQL.

## Isi

- [Fitur dan alur aplikasi](#fitur-dan-alur-aplikasi)
- [Arsitektur singkat](#arsitektur-singkat)
- [Persiapan dan setup lokal](#persiapan-dan-setup-lokal)
- [Menjalankan aplikasi](#menjalankan-aplikasi)
- [Testing](#testing)
- [Testing Flow](#testing-flow)
- [API utama](#api-utama)
- [Catatan keamanan dan konfigurasi](#catatan-keamanan-dan-konfigurasi)

## Fitur dan alur aplikasi

1. **Platform Admin** membuat akun pengguna dan grup, menambahkan anggota ke grup, mendaftarkan koneksi PostgreSQL, lalu mengatur izin grup untuk setiap database.
2. Pengguna login melalui halaman web. Backend memeriksa sesi dan izin saat request diproses.
3. Pada database yang dapat diakses, pengguna dapat melihat daftar schema, tabel, kolom, constraint, index, relasi Foreign Key, dan isi tabel.
4. Pengguna dengan izin **View + Edit** dapat membuat draft migration, mengubah draft, dan menjalankannya. Migration disimpan dalam riwayat platform dan SQL-nya dijalankan pada database target.

Izin diberikan kepada **grup**, bukan anggota satu per satu. Izin efektif pengguna adalah izin tertinggi dari grup-grup yang diikutinya: `view_edit` mengungguli `view_only`. Status Platform Admin memberikan hak mengelola platform, tetapi tidak otomatis memberikan izin ke setiap database target.

## Arsitektur singkat

```text
Browser
  ├── halaman HTML, CSS, dan JavaScript
  └── request API + cookie sesi + token CSRF
                 │
                 ▼
           Backend Go
  ├── Access Control: akun, grup, izin, sesi
  ├── Database Management: koneksi, schema viewer, data viewer
  └── Migration: draft, validasi urutan, eksekusi, riwayat
          │                         │
          ▼                         ▼
 PostgreSQL platform       PostgreSQL target
 (akun, sesi, konfigurasi,  (schema/data yang dikelola
  izin, riwayat migration)   dan tempat SQL migration berjalan)
```

- **Frontend** menggunakan Go templates, CSS, dan JavaScript yang disajikan langsung oleh backend. Tidak ada server frontend atau dependency Node.js yang perlu dijalankan.
- **Database platform** diatur oleh `DATABASE_URL`. Database ini menyimpan akun, sesi, grup, izin, konfigurasi koneksi database target, dan riwayat migration.
- **Database target** adalah database PostgreSQL yang didaftarkan melalui aplikasi. Viewer membacanya; migration menjalankan SQL ke database ini.
- **Access Control** memvalidasi sesi, memeriksa hak admin, dan menghitung izin database dari keanggotaan grup.
- **Database Management** menguji koneksi target, menampilkan schema/data, dan menyimpan konfigurasi koneksi. Password koneksi target disimpan terenkripsi AES-GCM.
- **Migration** menyimpan draft dan status pada database platform. Eksekusi SQL target menggunakan transaksi; versi migration per database dijalankan berurutan.

## Alur kerja tiap modul

### Access Control

1. Saat backend mulai, modul menyiapkan tabel Access Control. Jika konfigurasi bootstrap admin diisi, akun Platform Admin disinkronkan dari environment.
2. Saat login, backend mencari akun berdasarkan username dan mencocokkan password dengan hash bcrypt yang tersimpan. Password asli tidak disimpan.
3. Setelah berhasil login, backend membuat ID sesi acak dan token CSRF. Browser menerima ID sesi lewat cookie `HttpOnly`; database menyimpan hash ID sesi. Cookie CSRF dapat dibaca JavaScript agar frontend bisa mengirim nilainya pada header `X-CSRF-Token`.
4. Pada request berikutnya, middleware membaca cookie sesi dan mencari sesi yang masih aktif di PostgreSQL. Middleware memeriksa status akun, masa berlaku, idle timeout, lalu merotasi ID sesi sesuai kebijakan.
5. Untuk route admin, backend memeriksa status Platform Admin. Untuk route database, backend menghitung izin dari seluruh grup pengguna pada database tersebut. `view_edit` menjadi izin efektif jika setidaknya satu grup memberi akses itu; selainnya akses yang diberikan bisa berupa `view_only`.
6. Request yang mengubah data harus lolos pemeriksaan origin dan CSRF. Logout menghapus sesi aktif dan membersihkan cookie.

Karena pemeriksaan sesi dan izin dilakukan di backend, perubahan status akun, sesi, atau akses akan diterapkan pada request berikutnya. Tombol yang disembunyikan frontend hanya membantu tampilan; keputusan akses tetap dilakukan oleh backend.

### Database Management

1. Platform Admin mengisi identitas koneksi target: host, port, nama database, username, password, SSL mode, dan keterangan.
2. Backend memvalidasi input. Password target dienkripsi AES-GCM dengan `DATABASE_CREDENTIALS_KEY` sebelum disimpan di database platform; API tidak mengembalikan password.
3. Saat admin memilih **Uji Koneksi**, backend mengambil konfigurasi, mendekripsi password di memori, membuka atau memakai ulang pool koneksi target, lalu mencoba ping. Pesan kegagalan dibedakan sebisa mungkin, misalnya autentikasi ditolak, database tidak ditemukan, atau target tidak dapat dijangkau.
4. Pengguna yang membuka database melewati pemeriksaan sesi dan izin. Backend menggunakan koneksi target untuk membaca daftar schema, struktur tabel, constraint, index, relasi Foreign Key, dan baris data. Operasi target dibatasi jumlahnya agar permintaan bersamaan tidak membuka koneksi tanpa batas.
5. Pengubahan atau penghapusan konfigurasi koneksi menginvalidasi pool terkait. Penghapusan koneksi ditolak ketika masih ada migration berstatus berjalan.

Database platform dan database target memiliki peran berbeda: konfigurasi koneksi dan izin tersimpan di platform, sedangkan schema dan data yang ditampilkan berada di target.

### Migration

1. Pengguna dengan izin `view_edit` mengisi nama dan SQL migration. Backend memvalidasi input dan menyimpan migration sebagai `draft` di database platform dengan versi berikutnya untuk database target tersebut.
2. Draft dapat diedit sebelum dijalankan. Migration berstatus `failed` juga dapat diperbaiki dan dicoba kembali; migration yang sudah `applied` tidak bisa dijalankan ulang melalui alur biasa.
3. Saat tombol eksekusi ditekan, backend memeriksa izin edit, mengambil lock per database, membaca ulang status migration, lalu memastikan tidak ada versi sebelumnya yang belum `applied`. Ini menjaga urutan eksekusi dan mencegah dua eksekusi bersamaan untuk database yang sama.
4. Status dicatat sebagai `running`, kemudian SQL dijalankan dalam transaksi pada database target dengan batas waktu. Jika SQL gagal, transaksi di-rollback, status disimpan sebagai `failed`, dan riwayat menampilkan SQLSTATE serta pesan PostgreSQL.
5. Jika transaksi berhasil di-commit, status menjadi `applied`. Jika koneksi terputus saat commit sehingga hasilnya tidak bisa dipastikan, status dibiarkan `running` dan riwayat meminta operator memeriksa target sebelum mencoba ulang.

Draft dan riwayat migration berada di database platform, tetapi perubahan SQL berlangsung pada database target. Karena itu, status riwayat membantu melacak eksekusi, namun kondisi database target perlu diperiksa jika hasil commit tidak pasti.

## Persiapan dan setup lokal

### Kebutuhan

- Windows dengan PowerShell
- Docker Desktop dengan Docker Compose
- Go 1.22 atau lebih baru
- Browser modern

### 1. Siapkan PostgreSQL platform

Jalankan dari root repository:

```powershell
Copy-Item .env.example .env
docker compose up -d postgres
docker compose ps
```

Tunggu sampai status container PostgreSQL `healthy`. Port default host adalah `5432`. Jika port itu sedang dipakai, ubah `POSTGRES_PORT` di `.env` menjadi `5433`, lalu jalankan ulang `docker compose up -d postgres`. Gunakan port host yang sama dalam `DATABASE_URL` dan `TEST_DATABASE_URL`.

> Docker Compose membaca `.env` untuk konfigurasi container. Proses Go **tidak membaca `.env` otomatis**; variabel backend harus disetel di PowerShell tempat backend dijalankan.

### 2. Buat dan simpan kunci enkripsi

Buat kunci 32 byte acak satu kali. Salin hasil Base64 dan simpan dengan aman, misalnya di password manager:

```powershell
$credentialKeyBytes = New-Object byte[] 32
[System.Security.Cryptography.RandomNumberGenerator]::Fill($credentialKeyBytes)
[Convert]::ToBase64String($credentialKeyBytes)
```

Gunakan **kunci yang sama setiap kali** backend dijalankan. Jangan commit kunci ke Git. Jika kunci hilang atau diganti, password database target yang sudah terenkripsi tidak dapat dibaca lagi.

### 3. Set konfigurasi backend

Jalankan di PowerShell yang sama dengan perintah `go run`. Sesuaikan port database dan isi kunci hasil langkah sebelumnya:

```powershell
$env:DATABASE_URL = "postgres://platform:platform_dev_password@127.0.0.1:5432/platform_dev?sslmode=disable"
$env:DATABASE_CREDENTIALS_KEY = "tempel-hasil-kunci-base64-di-sini"
$env:PLATFORM_ADMIN_USERNAME = "admin"
$env:PLATFORM_ADMIN_PASSWORD = "ganti-dengan-password-kuat"
$env:BACKEND_PORT = "8080"
$env:COOKIE_SECURE = "false"
```

Password Platform Admin minimal 12 karakter. Untuk pengembangan lokal lewat HTTP, `COOKIE_SECURE=false` diperlukan agar browser mengirim cookie. Pada deployment HTTPS, aktifkan `COOKIE_SECURE=true`.

## Menjalankan aplikasi

Dari root repository:

```powershell
Set-Location backend
go mod download
go run ./cmd/api
```

Backend menyiapkan tabel yang dibutuhkan saat startup. Buka:

- `http://localhost:8080/login` untuk login.
- `http://localhost:8080/admin` untuk panel Platform Admin.
- `http://localhost:8080/health` untuk memastikan backend merespons.

Frontend dilayani backend yang sama, jadi tidak perlu menjalankan perintah frontend terpisah. Biarkan terminal backend tetap berjalan selama aplikasi dipakai. Hentikan dengan `Ctrl+C`.

### Bootstrap Platform Admin

Jika `PLATFORM_ADMIN_USERNAME` dan `PLATFORM_ADMIN_PASSWORD` diisi, backend membuat atau menyinkronkan akun dengan username tersebut saat startup. Password di-hash dengan bcrypt. Perubahan environment berlaku setelah backend dimulai ulang dan sesi akun itu dicabut. Jika username environment diganti, akun admin lama dengan username berbeda tidak otomatis dihapus atau dinonaktifkan. Jangan gunakan password contoh di atas untuk deployment.

### Menambahkan database target

1. Pastikan PostgreSQL target dapat dijangkau dari komputer yang menjalankan backend.
2. Login sebagai Platform Admin, buka **Panel Platform Admin**, isi host, port, nama database, username, password, dan SSL mode.
3. Tekan **Uji Koneksi** sebelum menyimpan atau gunakan tombol uji pada koneksi yang sudah tersimpan.
4. Berikan izin `View Only` atau `View + Edit` kepada grup yang tepat.

Jika PostgreSQL target berjalan pada Docker host yang sama, gunakan port host yang dipublikasikan. Contohnya, jika Compose memetakan `5433:5432`, isi host `127.0.0.1` dan port `5433`.

## Testing

Test di repository dibagi berdasarkan bagian aplikasi yang diperiksa. Label di komentar awal setiap file `*_test.go` menunjukkan kategorinya.

| Jenis | Cakupan di repository | Apa yang diperiksa |
|---|---|---|
| Unit | `password_test.go`, `permission_test.go`, `store_test.go`, `migration/store_test.go` | Fungsi kecil tanpa PostgreSQL, seperti hash password, normalisasi username/ID, validasi input, perhitungan izin tertinggi, dan konstruksi DSN. |
| HTTP/handler integration | `accesscontrol/http_test.go`, `databasemanagement/http_test.go`, `migration/http_test.go`, `web/handler_test.go` | Beberapa bagian dalam satu modul dirangkai melalui HTTP handler; memeriksa autentikasi, CSRF, izin route, validasi request, template, serta aset static. Sebagian memakai fake/mock untuk mengisolasi dependensi luar. |
| PostgreSQL integration | `store_integration_test.go` pada Access Control, Database Management, dan Migration | Store dan query nyata terhadap PostgreSQL: schema terisolasi, akun/grup/sesi, kredensial koneksi, viewer metadata, urutan migration, transaksi, rollback, dan riwayat. |
| End-to-end (HTTP API) | `internal/e2e/application_test.go` | Alur lintas modul pada server HTTP dengan PostgreSQL sungguhan: login admin, buat pengguna/grup/koneksi, atur izin, login pengguna, lihat schema/tabel, penolakan akses edit untuk `view_only`, perubahan izin langsung berlaku, jalankan migration, lihat kolom hasilnya, lalu logout. |
| Smoke | `cmd/api/main_test.go` | Pemeriksaan cepat bahwa router utama menghubungkan health check, halaman web, static asset, dan API dengan status akses yang diharapkan. |

End-to-end otomatis saat ini menguji alur dari sisi HTTP API, belum mengendalikan browser atau mengklik UI. Untuk tampilan dan interaksi browser, gunakan [Testing Flow](#testing-flow).

### Menjalankan unit, handler, smoke, dan build

Jalankan perintah dari folder `backend`:

```powershell
go test ./... -count=1
go build ./...
```

Tanpa `TEST_DATABASE_URL`, test PostgreSQL integration dan E2E akan dilewati dengan status `SKIP`; paket tetap dapat melaporkan `ok`. `-count=1` memaksa Go menjalankan ulang test, bukan memakai hasil cache.

### Menjalankan test per kategori

Semua test yang memerlukan PostgreSQL membaca environment `TEST_DATABASE_URL`. Pastikan PostgreSQL development lokal sudah aktif dan URL menunjuk ke database yang boleh dipakai test.

```powershell
$env:TEST_DATABASE_URL = "postgres://platform:platform_dev_password@127.0.0.1:5432/platform_dev?sslmode=disable"

# Semua PostgreSQL integration dan alur end-to-end HTTP
go test ./... -run 'Test(Postgres|ApplicationUserFlow)' -v -count=1

# Hanya end-to-end HTTP lintas modul
go test ./internal/e2e -run TestApplicationUserFlow -v -count=1
```

Untuk menjalankan **semua kategori sekaligus**, set `TEST_DATABASE_URL`, lalu jalankan:

```powershell
go test ./... -count=1
```

Test PostgreSQL membuat schema sementara dengan prefix `ac_test_`, `dm_test_`, `mg_test_`, atau `e2e_`, lalu membersihkannya. Modul Database Management dan E2E juga mendaftarkan koneksi ke database test agar viewer dan migration dapat terhubung. Gunakan database lokal khusus development, bukan database berisi data penting. Role PostgreSQL perlu izin membuat dan menghapus schema.

## Testing Flow

Gunakan database target sementara yang berisi data contoh, misalnya tabel `customers` dan `orders`. Jangan menguji migration destruktif pada database yang menyimpan data penting.

1. Login sebagai Platform Admin, daftarkan koneksi PostgreSQL target, lalu pilih **Uji Koneksi**.
2. Buat pengguna biasa dan grup. Tambahkan pengguna sebagai anggota grup, lalu berikan grup akses **View Only** ke database target.
3. Login sebagai pengguna tersebut. Buka database dan periksa schema, tabel, kolom, constraint, index, data, serta relasi Foreign Key pada **Schema Visualizer**.
4. Buka `/admin` sebagai pengguna biasa dan pastikan halaman menampilkan peringatan bahwa akses hanya untuk Platform Admin. Pada database dengan akses View Only, pastikan pengguna dapat melihat migration tetapi tidak menjalankannya.
5. Sebagai admin, ubah izin grup menjadi **View + Edit**. Muat ulang database sebagai pengguna biasa, buat draft migration, lalu simpan dan jalankan. Contoh:

   ```sql
   ALTER TABLE public.customers ADD COLUMN test_note TEXT;
   ```

   Pastikan kolom muncul di viewer dan migration tercatat berhasil.

6. Uji rollback dengan SQL yang gagal setelah membuat perubahan:

   ```sql
   CREATE TABLE public.rollback_probe (id INT PRIMARY KEY);
   SELECT no_such_migration_function();
   ```

   Pastikan migration berstatus gagal dan tabel `rollback_probe` tidak terbentuk. Error menampilkan SQLSTATE dan pesan PostgreSQL. Perbaiki draft yang sama, misalnya menjadi `SELECT 1;`, lalu jalankan agar versi berikutnya tidak tertahan.

7. Sebagai admin, cabut izin grup atau keluarkan pengguna dari grup. Muat ulang halaman sebagai pengguna tersebut dan pastikan database tidak lagi dapat diakses.

## API utama

### Access Control

- `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me`
- `GET /api/admin/users`, `POST /api/admin/users`
- Endpoint admin untuk password, status aktif, hak Platform Admin, grup, anggota, dan izin database.
- `GET /api/auth/databases/{databaseID}/access`

### Database Management

- Endpoint `/api/admin/databases...` untuk mengelola dan menguji koneksi.
- Endpoint `/api/databases/{databaseID}/...` untuk daftar schema, detail tabel, dan data.

### Migration

- `GET /api/databases/{databaseID}/migrations` dan `GET /api/databases/{databaseID}/migrations/{migrationID}` untuk melihat draft/riwayat (perlu `view_only`).
- `POST /api/databases/{databaseID}/migrations` untuk membuat draft.
- `PUT /api/databases/{databaseID}/migrations/{migrationID}` untuk mengubah draft atau migration gagal.
- `POST /api/databases/{databaseID}/migrations/{migrationID}/execute` untuk menjalankan SQL (perlu `view_edit`).

Request yang mengubah data juga harus mengirim token CSRF. Rute dan format lengkap tersedia di handler masing-masing modul pada `backend/internal/`.

## Catatan keamanan dan konfigurasi

- Password pengguna disimpan sebagai hash bcrypt, bukan teks biasa.
- ID sesi acak dikirim melalui cookie `HttpOnly`; database menyimpan hash ID sesi. Sesi mendukung idle timeout 30 menit, masa berlaku maksimum 12 jam, rotasi tiap 15 menit, dan grace period 30 detik.
- Cookie sesi memakai `SameSite=Lax`; cookie CSRF dipakai bersama header `X-CSRF-Token` untuk request perubahan data. `Secure` perlu diaktifkan saat menggunakan HTTPS.
- Izin dicek di backend pada setiap request. Menyembunyikan tombol di frontend bukan pengganti pemeriksaan izin server.
- Password koneksi target dienkripsi dengan AES-GCM menggunakan `DATABASE_CREDENTIALS_KEY`. Baris password lama dienkripsi saat startup.
- Migration berukuran maksimal 256 KiB dan memiliki batas eksekusi dua menit. Error SQL menyertakan SQLSTATE dan pesan PostgreSQL; jika hasil commit tidak pasti karena koneksi terputus, periksa database target sebelum menjalankan ulang.
- Jangan mengirim cookie sesi melalui URL atau menyimpan ID sesi di `localStorage`.

Aturan lebih rinci tersedia di `backend/internal/accesscontrol/POLICY.md`, `backend/internal/databasemanagement/POLICY.md`, dan `backend/internal/migration/POLICY.md`.
