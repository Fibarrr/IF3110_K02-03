# Aturan akses

- Pengguna mendapat akses database lewat keanggotaan grup, bukan lewat izin yang diberikan langsung ke pengguna.
- Untuk setiap database, grup bisa mendapat akses `View Only` atau `View + Edit`.
- Kalau beberapa grup memberi izin berbeda, izin tertinggi yang berlaku. Jadi, `View + Edit` menang atas `View Only`.
- Tanpa izin, pengguna tidak bisa mengakses database. Peran Platform Admin untuk mengelola platform dan tidak otomatis memberi akses ke database.
- Username disimpan dalam huruf kecil dan hanya boleh berisi huruf, angka, titik, garis bawah, atau tanda hubung; panjangnya 3–64 karakter.
- Password minimal 12 karakter dan maksimal 72 byte, lalu di-hash dengan bcrypt.

## Aturan sesi

- Sesi tidak aktif selama 30 menit akan berakhir.
- Sesi berakhir maksimal 12 jam setelah login, meskipun masih aktif.
- ID sesi dirotasi saat login, saat akses pengguna berubah, dan setiap 15 menit.
- ID sesi lama masih diterima selama 30 detik setelah rotasi untuk menangani request yang berjalan bersamaan.
- Rotasi tidak memperpanjang batas umur 12 jam.
- Cookie sesi memakai `HttpOnly` dan `SameSite=Lax`; `Secure` aktif saat server dikonfigurasi untuk HTTPS.
- Request yang mengubah data wajib menyertakan token CSRF.

## Leader grup untuk approval migration

- Setiap grup dapat memiliki satu leader, dan leader harus merupakan anggota grup yang aktif.
- Platform Admin menetapkan atau mengganti leader melalui `PUT /api/admin/groups/{groupID}/leader` dengan body `{"userId": 123}`. Endpoint mengikuti autentikasi, otorisasi Platform Admin, dan pemeriksaan CSRF yang sama seperti perubahan Access Control lainnya.
- `GET /api/admin/groups` menyertakan `leaderUserId` dan `leaderUsername`. Nilai leader dapat `null` untuk grup lama atau grup yang belum ditetapkan; UI harus meminta admin menetapkan leader sebelum grup ikut dalam approval.
- `Store.ListDatabaseApprovers(ctx, databaseID)` mengembalikan semua grup yang memiliki grant ke database, termasuk `view_only`, beserta leader aktifnya. Baris tanpa leader valid memiliki `leaderUserId: null`; modul Migration harus menahan pengajuan/eksekusi yang membutuhkan approval sampai admin menetapkan leader.
- Leader tidak dapat dikeluarkan dari grup atau dinonaktifkan sebelum diganti. Ganti leader terlebih dahulu agar tidak ada grup dengan leader yang tidak aktif.
