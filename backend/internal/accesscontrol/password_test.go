package accesscontrol

import "testing"

func TestPasswordHash(t *testing.T) {
	password := "contoh-password-kuat-2026"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if hash == password {
		t.Fatal("password tersimpan apa adanya")
	}
	if !CheckPassword(password, hash) {
		t.Fatal("password yang benar tidak cocok")
	}
	if CheckPassword("password-yang-salah", hash) {
		t.Fatal("password yang salah malah cocok")
	}
	if _, err := HashPassword("pendek"); err == nil {
		t.Fatal("password pendek seharusnya ditolak")
	}
}

func TestNormalizeUsername(t *testing.T) {
	got, err := normalizeUsername("  Fibar_01 ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "fibar_01" {
		t.Fatalf("normalizeUsername() = %q, mau %q", got, "fibar_01")
	}
	if _, err := normalizeUsername("nama pengguna"); err == nil {
		t.Fatal("username dengan spasi seharusnya ditolak")
	}
}
