package accesscontrol

import (
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 64 {
		return "", InputError("username harus berisi 3 sampai 64 karakter")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '_' && r != '-' {
			return "", InputError("username hanya boleh berisi huruf, angka, titik, garis bawah, dan tanda hubung")
		}
	}
	return username, nil
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", InputError("password minimal 12 karakter")
	}
	if len(password) > 72 {
		return "", InputError("password maksimal 72 byte")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func CheckPassword(password, hash string) bool {
	return len(password) <= 72 && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
