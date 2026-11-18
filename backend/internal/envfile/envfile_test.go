package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# comment\r\nDATABASE_URL=postgres://u:p@localhost:5432/db?sslmode=disable\r\nPLATFORM_ADMIN_USERNAME=\"admin\"\r\nCOOKIE_SECURE='false'\r\nEXISTING_KEY=from_file\r\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatalf("tulis .env gagal: %v", err)
	}

	t.Setenv("EXISTING_KEY", "from_env")
	for _, k := range []string{"DATABASE_URL", "PLATFORM_ADMIN_USERNAME", "COOKIE_SECURE"} {
		if prev, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, prev) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}

	Load(filepath.Join(dir, "missing.env"), envPath)

	if got := os.Getenv("DATABASE_URL"); got != "postgres://u:p@localhost:5432/db?sslmode=disable" {
		t.Fatalf("DATABASE_URL = %q", got)
	}
	if got := os.Getenv("PLATFORM_ADMIN_USERNAME"); got != "admin" {
		t.Fatalf("PLATFORM_ADMIN_USERNAME = %q", got)
	}
	if got := os.Getenv("COOKIE_SECURE"); got != "false" {
		t.Fatalf("COOKIE_SECURE = %q", got)
	}
	if got := os.Getenv("EXISTING_KEY"); got != "from_env" {
		t.Fatalf("EXISTING_KEY overwritten: %q", got)
	}
}
