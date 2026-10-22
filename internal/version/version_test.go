package version

import (
	"testing"
)

func TestString(t *testing.T) {
	got := String()
	expected := "plainbucket dev (commit: unknown, built: unknown)"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestStringWithValues(t *testing.T) {
	origVersion, origCommit, origDate := Version, GitCommit, BuildDate
	t.Cleanup(func() {
		Version = origVersion
		GitCommit = origCommit
		BuildDate = origDate
	})

	Version = "v1.2.3"
	GitCommit = "abc1234"
	BuildDate = "2026-01-01T00:00:00Z"

	got := String()
	expected := "plainbucket v1.2.3 (commit: abc1234, built: 2026-01-01T00:00:00Z)"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestDefaultValues(t *testing.T) {
	if Version != "dev" {
		t.Errorf("expected default Version 'dev', got %q", Version)
	}
	if GitCommit != "unknown" {
		t.Errorf("expected default GitCommit 'unknown', got %q", GitCommit)
	}
	if BuildDate != "unknown" {
		t.Errorf("expected default BuildDate 'unknown', got %q", BuildDate)
	}
}
