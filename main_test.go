package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sendSignalAfter delivers sig to this process after d.
func sendSignalAfter(t *testing.T, d time.Duration, sig syscall.Signal) {
	t.Helper()
	go func() {
		time.Sleep(d)
		_ = syscall.Kill(os.Getpid(), sig)
	}()
}

func TestRun_CommitOnSignal(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")

	sendSignalAfter(t, 100*time.Millisecond, syscall.SIGUSR1)

	code := run([]string{
		"--timeout", "5",
		"--command", "touch " + marker,
		"--rollback-command", "rm -f " + marker,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("command did not run: %v", err)
	}
}

func TestRun_RollbackOnTimeout(t *testing.T) {
	dir := t.TempDir()
	rolled := filepath.Join(dir, "rolled")

	code := run([]string{
		"--timeout", "1",
		"--command", "true",
		"--rollback-command", "touch " + rolled,
	})
	if code != 100 {
		t.Fatalf("exit code = %d, want 100 (default timeout exit code)", code)
	}
	if _, err := os.Stat(rolled); err != nil {
		t.Fatalf("rollback command did not run: %v", err)
	}
}

func TestRun_FileSnapshotRestoredOnRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := run([]string{
		"--timeout", "1",
		"--command", "echo modified > " + target,
		"--files", target,
		"--rollback-command", "true",
	})
	if code != 100 {
		t.Fatalf("exit code = %d, want 100 (default timeout exit code)", code)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("file content = %q, want %q", got, "original")
	}
}

func TestRun_CreatedFileRemovedOnRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "new.txt")

	code := run([]string{
		"--timeout", "1",
		"--command", "echo hi > " + target,
		"--files", target,
	})
	if code != 100 {
		t.Fatalf("exit code = %d, want 100 (default timeout exit code)", code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file should have been removed on rollback, stat err = %v", err)
	}
}

func TestRun_FileSnapshotKeptOnCommit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	sendSignalAfter(t, 100*time.Millisecond, syscall.SIGUSR1)

	code := run([]string{
		"--timeout", "5",
		"--command", "printf committed > " + target,
		"--files", target,
		"--rollback-command", "true",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "committed" {
		t.Fatalf("file content = %q, want %q", got, "committed")
	}
}

func TestRun_CommandFailureExitsWithoutRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolled := filepath.Join(dir, "rolled")

	// Command modifies the file then exits non-zero. Default behavior: fail
	// immediately, propagate the exit code, and do NOT roll back.
	code := run([]string{
		"--timeout", "5",
		"--command", "echo modified > " + target + "; exit 3",
		"--files", target,
		"--rollback-command", "touch " + rolled,
	})
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (command's own code)", code)
	}
	if got, _ := os.ReadFile(target); string(got) != "modified\n" {
		t.Fatalf("file should NOT be restored, content = %q", got)
	}
	if _, err := os.Stat(rolled); !os.IsNotExist(err) {
		t.Fatalf("rollback command should NOT have run, stat err = %v", err)
	}
}

func TestRun_CommandFailureRollsBackWithFlag(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolled := filepath.Join(dir, "rolled")

	code := run([]string{
		"--timeout", "5",
		"--command", "echo modified > " + target + "; exit 3",
		"--files", target,
		"--rollback-command", "touch " + rolled,
		"--rollback-on-failure",
	})
	if code != 99 {
		t.Fatalf("exit code = %d, want 99 (default rollback-on-failure exit code)", code)
	}
	if got, _ := os.ReadFile(target); string(got) != "original" {
		t.Fatalf("file should be restored, content = %q", got)
	}
	if _, err := os.Stat(rolled); err != nil {
		t.Fatalf("rollback command did not run: %v", err)
	}
}

func TestRun_CustomTimeoutExitCode(t *testing.T) {
	code := run([]string{
		"--timeout", "1",
		"--command", "true",
		"--timeout-exit-code", "42",
	})
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestRun_RollbackCommandFailureUsesRollbackFailCode(t *testing.T) {
	// Timeout triggers rollback, but the rollback command itself fails: the exit
	// code must be the dedicated rollback-failure code, not the timeout code.
	code := run([]string{
		"--timeout", "1",
		"--command", "true",
		"--rollback-command", "exit 7",
		"--timeout-exit-code", "100",
		"--rollback-failure-exit-code", "101",
	})
	if code != 101 {
		t.Fatalf("exit code = %d, want 101 (rollback failure code)", code)
	}
}

func TestRun_CommitOnCustomSignal(t *testing.T) {
	sendSignalAfter(t, 100*time.Millisecond, syscall.SIGUSR2)

	code := run([]string{
		"--timeout", "5",
		"--command", "true",
		"--signal", "SIGUSR2",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestRun_UnsupportedSignal(t *testing.T) {
	if code := run([]string{"--timeout", "5", "--signal", "BOGUS"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestParseSignal(t *testing.T) {
	cases := map[string]syscall.Signal{
		"USR1":      syscall.SIGUSR1,
		"usr1":      syscall.SIGUSR1,
		"SIGUSR2":   syscall.SIGUSR2,
		" sigterm ": syscall.SIGTERM,
	}
	for in, want := range cases {
		got, err := parseSignal(in)
		if err != nil {
			t.Errorf("parseSignal(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSignal(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseSignal("nope"); err == nil {
		t.Error("parseSignal(\"nope\") expected error, got nil")
	}
}

func TestRun_TimeoutRequired(t *testing.T) {
	if code := run([]string{"--command", "true"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestStringSlice(t *testing.T) {
	var s stringSlice
	_ = s.Set("a, b")
	_ = s.Set("c")
	if got := s.String(); got != "a,b,c" {
		t.Fatalf("got %q, want %q", got, "a,b,c")
	}
}
