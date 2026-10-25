# Aturan migration

- Draft dan riwayat disimpan di database platform; SQL dijalankan ke database target yang dipilih.
- Versi dibuat berurutan per database saat draft dibuat. Migration versi berikutnya baru bisa dijalankan setelah semua versi sebelumnya sukses.
- Pengguna dengan akses `view_only` bisa melihat SQL dan riwayat. Membuat, mengubah, atau menjalankan migration butuh `view_edit` serta token CSRF.
- Draft dan migration yang gagal bisa diubah. Migration yang sukses tidak bisa diedit atau dijalankan ulang.
- SQL dijalankan dalam satu transaksi PostgreSQL. Kalau ada error, transaksi di-rollback, status menjadi `failed`, dan versi berikutnya tertahan sampai migration itu diperbaiki lalu dijalankan.
- SQL dibatasi maksimal 256 KiB dan waktu eksekusi maksimal 2 menit. Role PostgreSQL target harus punya izin untuk menjalankan SQL yang diberikan.
- Riwayat mencatat versi, SQL, pembuat, waktu pembuatan, pelaksana, waktu eksekusi, dan status.
- Status `running` yang tertinggal setelah proses backend mati perlu diperiksa manual sebelum dilanjutkan, karena transaksi target mungkin sudah selesai sementara status belum sempat diperbarui.
- Kalau koneksi database dihapus dari platform, migration dan riwayatnya ikut terhapus.

## Endpoint

- `GET /api/databases/{databaseID}/migrations` — lihat SQL draft dan riwayat.
- `GET /api/databases/{databaseID}/migrations/{migrationID}` — lihat satu migration.
- `POST /api/databases/{databaseID}/migrations` — buat draft dengan body `{"name":"...","sql":"..."}`.
- `PUT /api/databases/{databaseID}/migrations/{migrationID}` — ubah draft atau migration gagal.
- `POST /api/databases/{databaseID}/migrations/{migrationID}/execute` — jalankan migration.
