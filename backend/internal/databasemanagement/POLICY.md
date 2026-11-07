# Aturan Database Management

- Konfigurasi koneksi PostgreSQL disimpan di tabel `dm_databases` pada database platform dan hanya dapat dikelola oleh **Platform Admin** melalui endpoint `/api/admin/databases` dengan proteksi sesi serta token CSRF.
- Password database target dienkripsi dengan AES-GCM sebelum disimpan di database platform; kuncinya harus disediakan melalui `DATABASE_CREDENTIALS_KEY` dan disimpan terpisah dari database. Password **tidak pernah** dikembalikan pada respons JSON API (hanya boolean `hasPassword`) maupun pesan error.
- Saat **Platform Admin** memperbarui koneksi (`PUT /api/admin/databases/{databaseID}`) dengan field `password` kosong, password lama tetap dipertahankan.
- Saat koneksi database dihapus (`DELETE /api/admin/databases/{databaseID}`), penghapusan ditolak jika masih ada migration berstatus `running`. Penghapusan koneksi, pencabutan izin grup, dan penandaan rotasi sesi terdampak dijalankan secara atomik dalam satu transaksi SQL.
- Seluruh akses ke database target wajib melalui pemeriksaan izin efektif grup (`accesscontrol.AccessViewOnly` atau `accesscontrol.AccessViewEdit`). Sesuai aturan Access Control, peran **Platform Admin** tidak otomatis memberi izin akses ke isi database tanpa grant grup.
- Database viewer hanya menampilkan schema pengguna (menyembunyikan schema internal PostgreSQL: `pg_catalog`, `information_schema`, `pg_toast*`, dan `pg_temp*`) serta menyediakan informasi schema, tabel/view, kolom (termasuk Primary Key), constraint (termasuk relasi Foreign Key untuk Schema Visualizer), index, dan pembacaan baris data dengan paginasi (`limit` dan `offset`) di dalam transaksi `READ ONLY`.
- Operasi database target memakai pool koneksi yang digunakan ulang, dengan maksimal delapan operasi target berjalan bersamaan.
- Kegagalan koneksi ke database target ditangani dengan mengembalikan status HTTP `502 Bad Gateway` beserta penyebab umum yang telah disanitasi tanpa membocorkan password ataupun string DSN.
