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
