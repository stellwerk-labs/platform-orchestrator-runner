package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalCommandCancellationStopsChildProcesses(t *testing.T) {
	late := filepath.Join(t.TempDir(), "late-effect")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runLocalCommand(ctx, "/bin/sh", "-c", `trap '' INT; (sleep 1; touch "$1") & wait`, "sh", late)
	if err == nil {
		t.Fatal("canceled local command succeeded")
	}
	time.Sleep(800 * time.Millisecond)
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatal("child process survived local command cancellation")
	}
}
