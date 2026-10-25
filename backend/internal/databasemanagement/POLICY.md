# Aturan pengelolaan database

- Hanya Platform Admin yang dapat menambah, mengubah, menguji, dan menghapus konfigurasi koneksi database PostgreSQL melalui endpoint `/api/admin/databases`.
- Setiap database yang terdaftar memiliki `id` unik (huruf kecil, angka, titik, garis bawah, atau tanda hubung; 1–64 karakter) yang terhubung dengan izin grup pada modul Access Control (`ac_database_grants.database_id`).
- Password koneksi database target tidak pernah ditampilkan kembali pada URL, response API, maupun log aplikasi.
- Saat Platform Admin memperbarui konfigurasi koneksi (`PUT /api/admin/databases/{databaseID}`), pengosongan field `password` akan mempertahankan password yang sudah tersimpan.
- Peran Platform Admin tidak otomatis memberi akses untuk melihat isi database. Pemeriksaan schema, tabel, kolom, constraint, index, dan data pada `/api/databases/{databaseID}/...` tetap mewajibkan izin `view_only` atau `view_edit` dari keanggotaan grup pengguna.

## Aturan database viewer dan penanganan error

- Daftar schema dan tabel mengecualikan schema internal PostgreSQL (`pg_catalog`, `information_schema`, `pg_toast*`, dan `pg_temp*`).
- Pembacaan baris data tabel (`GET /api/databases/{databaseID}/schemas/{schemaName}/tables/{tableName}/rows`) wajib memverifikasi keberadaan schema dan tabel terlebih dahulu di katalog PostgreSQL, mengutip identifier dengan aman (`pq.QuoteIdentifier`), dan berjalan di dalam transaksi `READ ONLY` dengan batas waktu 5 detik.
- Paginasi pembacaan data tabel dibatasi dengan `limit` default 50 (minimum 1, maksimum 200) dan `offset` minimum 0.
- Perubahan baris hanya tersedia untuk akses `view_edit`, wajib lolos pemeriksaan CSRF, memvalidasi tabel/kolom lewat katalog PostgreSQL, dan mengutip seluruh identifier. Nilai disalurkan sebagai parameter query.
- Edit dan hapus baris hanya diizinkan pada tabel yang memiliki primary key; seluruh kolom primary key dipakai sebagai syarat identifikasi baris. Kolom identity dan generated tidak dapat diisi langsung.
- Perubahan struktur dari UI dibuat sebagai draft migration dan tidak dijalankan otomatis.
- Kegagalan koneksi ke database target dikembalikan sebagai `502 Bad Gateway` dengan pesan error yang telah disanitasi sehingga password atau DSN tidak pernah bocor ke client.
