package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"socialpublisher/internal/buildinfo"
)

func TestExpectedBuildRevisionIsCheckedBeforeSideEffects(t *testing.T) {
	previous := buildinfo.Revision
	t.Cleanup(func() { buildinfo.Revision = previous })
	buildinfo.Revision = strings.Repeat("a", 40)
	t.Setenv("EXPECTED_BUILD_REVISION", strings.Repeat("b", 40))
	t.Setenv("DATABASE_URL", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, mode := range []string{"serve", "api", "worker", "migrate"} {
		t.Run(mode, func(t *testing.T) {
			if err := run(context.Background(), []string{mode}, logger); err == nil || !strings.Contains(err.Error(), "build revision") {
				t.Fatalf("mode %s reached configuration or database before release guard: %v", mode, err)
			}
		})
	}
}
