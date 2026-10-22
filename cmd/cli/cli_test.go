package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// reset clears flag state a previous Execute left behind, such as --help.
func reset(t *testing.T, args ...string) {
	t.Helper()
	_ = rootCmd.Flags().Set("help", "false")
	rootCmd.SetArgs(args)
}

func TestHelp(t *testing.T) {
	t.Cleanup(func() { reset(t) })
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{"--help"})
	if err := Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "readable without the server") {
		t.Errorf("help = %q", buf.String())
	}
}

func TestInvalidConfig(t *testing.T) {
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "")
	reset(t)
	if err := Execute(); err == nil || !strings.Contains(err.Error(), "ACCESS_KEY") {
		t.Errorf("err = %v", err)
	}
}

func TestServeUntilCancelled(t *testing.T) {
	t.Setenv("PLAINBUCKET_ACCESS_KEY", "ak")
	t.Setenv("PLAINBUCKET_SECRET_KEY", "sk")
	t.Setenv("PLAINBUCKET_ROOT", t.TempDir())
	t.Setenv("PLAINBUCKET_ADDRESS", "127.0.0.1:0")
	reset(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rootCmd.ExecuteContext(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop when its context was cancelled")
	}
}

func TestVersion(t *testing.T) {
	reset(t, "version")
	if err := Execute(); err != nil {
		t.Fatal(err)
	}
}
