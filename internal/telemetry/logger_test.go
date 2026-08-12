package telemetry

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr temporarily replaces os.Stderr with a pipe, runs fn, and
// returns whatever was written to it. Tests here are not parallel because
// os.Stderr is process-global.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = original
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestOpenLogsWritesOnlyToFile(t *testing.T) {
	dir := t.TempDir()
	captured := captureStderr(t, func() {
		logs, err := OpenLogs(dir, "info")
		if err != nil {
			t.Fatal(err)
		}
		logs.Logger(RoleMCP).Info("file only marker")
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(captured, "file only marker") {
		t.Errorf("OpenLogs wrote to stderr: %q", captured)
	}
	content, err := os.ReadFile(filepath.Join(dir, RoleMCP+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "file only marker") {
		t.Errorf("log file missing entry: %q", content)
	}
}

func TestOpenLogsTeeStderrWritesToBoth(t *testing.T) {
	dir := t.TempDir()
	captured := captureStderr(t, func() {
		logs, err := OpenLogsTeeStderr(dir, "info")
		if err != nil {
			t.Fatal(err)
		}
		logs.Logger(RoleMCP).Info("tee marker")
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(captured, "tee marker") {
		t.Errorf("OpenLogsTeeStderr did not copy to stderr: %q", captured)
	}
	content, err := os.ReadFile(filepath.Join(dir, RoleMCP+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "tee marker") {
		t.Errorf("log file missing entry: %q", content)
	}
}

func TestStderrLogsWritesOnlyToStderrAndNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	captured := captureStderr(t, func() {
		logs := StderrLogs("info")
		logs.Logger(RoleMCP).Info("stderr only marker")
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(captured, "stderr only marker") {
		t.Errorf("StderrLogs did not write to stderr: %q", captured)
	}
	if _, err := os.Stat(filepath.Join(dir, RoleMCP+".log")); err == nil {
		t.Error("StderrLogs unexpectedly created a log file")
	}
}
