package config

import (
	"socialpublisher/internal/store"
	"strings"
	"testing"
)

func nativeEnv(t *testing.T) {
	env(t)
	t.Setenv("STORAGE_BACKEND", "cloudflare-do")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("NATIVE_STORAGE_URL", store.NativeStorageURL)
	t.Setenv("NATIVE_STORAGE_SESSION", strings.Repeat("a", 32))
}

func TestNativeConfigNeedsNoPostgresAndPreservesQueueLimit(t *testing.T) {
	nativeEnv(t)
	t.Setenv("PENDING_TARGETS_PER_OWNER", "321")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.StorageBackend != "cloudflare-do" || c.DatabaseURL != "" || c.NativeStorageURL != store.NativeStorageURL || c.PendingTargetsPerOwner != 321 {
		t.Fatal("native storage config lost native selection or queue bound")
	}
}
func TestNativeConfigFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, value string }{{"STORAGE_BACKEND", "d1"}, {"NATIVE_STORAGE_URL", "https://internet.example/v1"}, {"NATIVE_STORAGE_SESSION", ""}, {"DATABASE_URL", "postgres://private:never-print@example/database"}} {
		t.Run(tc.name, func(t *testing.T) {
			nativeEnv(t)
			t.Setenv(tc.name, tc.value)
			_, err := Load()
			if err == nil {
				t.Fatal("accepted ambiguous or unsafe native config")
			}
			if strings.Contains(err.Error(), "never-print") {
				t.Fatal("storage config exposed credentials")
			}
		})
	}
}
