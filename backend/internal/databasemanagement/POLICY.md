# Aturan Database Management

- Konfigurasi koneksi PostgreSQL disimpan di tabel `dm_databases` pada database platform dan hanya dapat dikelola oleh **Platform Admin** melalui endpoint `/api/admin/databases` dengan proteksi sesi serta token CSRF.
- Password database target digunakan oleh backend untuk membuka koneksi ke PostgreSQL target dan **tidak pernah** dikembalikan pada respons JSON API (`json:"-"`, hanya diekspos sebagai boolean `hasPassword`) maupun pesan error.
- Saat **Platform Admin** memperbarui koneksi (`PUT /api/admin/databases/{databaseID}`) dengan field `password` kosong, password lama tetap dipertahankan.
- Saat koneksi database dihapus (`DELETE /api/admin/databases/{databaseID}`), penghapusan data koneksi pada `dm_databases`, pencabutan seluruh izin grup (`ac_database_grants`), dan penandaan rotasi sesi pengguna terdampak (`ac_sessions`) dijalankan secara atomik dalam satu transaksi SQL.
- Seluruh akses ke database target wajib melalui pemeriksaan izin efektif grup (`accesscontrol.AccessViewOnly` atau `accesscontrol.AccessViewEdit`). Sesuai aturan Access Control, peran **Platform Admin** tidak otomatis memberi izin akses ke isi database tanpa grant grup.
- Database viewer hanya menampilkan schema pengguna (menyembunyikan schema internal PostgreSQL: `pg_catalog`, `information_schema`, `pg_toast*`, dan `pg_temp*`) serta menyediakan informasi schema, tabel/view, kolom (termasuk Primary Key), constraint (termasuk relasi Foreign Key untuk Schema Visualizer), index, dan pembacaan baris data dengan paginasi (`limit` dan `offset`) di dalam transaksi `READ ONLY`.
- Kegagalan koneksi ke database target ditangani dengan mengembalikan status HTTP `502 Bad Gateway` beserta pesan aman yang telah disanitasi tanpa membocorkan password ataupun string DSN.
