// Command c-txn is a transactional command runner.
//
// It runs a command and then waits for a confirmation signal (SIGUSR1) within a
// timeout. If the signal arrives in time, the "transaction" is committed and the
// process exits 0. If the timeout elapses without the signal, the transaction is
// rolled back: any file snapshots taken before the command ran are restored, the
// rollback command (if any) is executed, and the process exits 1.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// stringSlice is a flag.Value that accumulates repeated flags and also accepts
// comma-separated values within a single flag occurrence.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// fileSnapshot captures the state of a single file at snapshot time so it can be
// restored on rollback.
type fileSnapshot struct {
	path    string      // absolute path of the original file
	existed bool        // whether the file existed when the snapshot was taken
	mode    os.FileMode // original permission bits (valid only when existed)
	backup  string      // path to the backed-up copy (valid only when existed)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("c-txn", flag.ContinueOnError)

	var (
		timeout                 int
		command                 string
		rollbackCommand         string
		files                   stringSlice
		rollbackOnFailure       bool
		signalName              string
		timeoutExitCode         int
		rollbackFailCode        int
		commandFailRollbackCode int
	)
	fs.IntVar(&timeout, "timeout", 0, "seconds to wait for the confirmation signal before rolling back (required, > 0)")
	fs.StringVar(&command, "command", "", "command to run (optional)")
	fs.StringVar(&rollbackCommand, "rollback-command", "", "command to run when rolling back")
	fs.Var(&files, "files", "files to snapshot and restore on rollback (repeatable, comma-separated)")
	fs.BoolVar(&rollbackOnFailure, "rollback-on-failure", false, "roll back when the command itself exits non-zero (default: exit without rolling back)")
	fs.StringVar(&signalName, "signal", "USR1", "confirmation signal to wait for, e.g. USR1, USR2, HUP, INT, TERM (with or without the SIG prefix)")
	fs.IntVar(&timeoutExitCode, "timeout-exit-code", 100, "exit code to use when the timeout elapses without the confirmation signal")
	fs.IntVar(&rollbackFailCode, "rollback-failure-exit-code", 101, "exit code to use when the rollback itself fails (snapshot restore or rollback command errored)")
	fs.IntVar(&commandFailRollbackCode, "rollback-on-failure-exit-code", 99, "exit code to use when --command fails and --rollback-on-failure rolls back successfully")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if timeout <= 0 {
		fmt.Fprintln(os.Stderr, "c-txn: --timeout must be a positive integer (seconds)")
		return 2
	}
	sig, err := parseSignal(signalName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "c-txn: %v\n", err)
		return 2
	}

	// Register the signal handler as early as possible so that a signal that
	// arrives while the command is still running is not missed. The channel is
	// buffered so the notification is retained until we are ready to read it.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, sig)
	defer signal.Stop(sigCh)

	// The timeout budget starts now and covers the whole transaction, including
	// the time spent running the command.
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)

	// Take snapshots before running the command so the pre-command state can be
	// restored on rollback.
	var (
		snaps   []fileSnapshot
		snapDir string
	)
	if len(files) > 0 {
		var err error
		snapDir, err = os.MkdirTemp("", "c-txn-snapshot-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "c-txn: failed to create snapshot directory: %v\n", err)
			return 2
		}
		defer func() { _ = os.RemoveAll(snapDir) }()

		snaps, err = takeSnapshots(files, snapDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "c-txn: failed to snapshot files: %v\n", err)
			return 2
		}
	}

	// Run the command. By default a non-zero command exit fails immediately
	// without rolling back; with --rollback-on-failure it triggers a rollback.
	if command != "" {
		if err := runShell(command); err != nil {
			if rollbackOnFailure {
				fmt.Fprintf(os.Stderr, "c-txn: command failed (%v); rolling back\n", err)
				if rollback(snaps, rollbackCommand) != nil {
					return rollbackFailCode
				}
				return commandFailRollbackCode
			}
			fmt.Fprintf(os.Stderr, "c-txn: command failed (%v); exiting without rollback\n", err)
			return exitCode(err)
		}
	}

	// Wait for the confirmation signal or the deadline.
	if waitForSignal(sigCh, deadline) {
		// Committed: keep all changes.
		return 0
	}

	// Rolled back: restore snapshots first, then run the rollback command.
	fmt.Fprintf(os.Stderr, "c-txn: no SIG%s received within timeout; rolling back\n", canonicalSignalName(signalName))
	if rollback(snaps, rollbackCommand) != nil {
		return rollbackFailCode
	}
	return timeoutExitCode
}

// signalNames maps accepted signal names (uppercase, without the SIG prefix) to
// their syscall.Signal value.
var signalNames = map[string]syscall.Signal{
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"TERM": syscall.SIGTERM,
	"QUIT": syscall.SIGQUIT,
}

// canonicalSignalName normalizes a signal name to the map's key form: trimmed,
// uppercased, without the SIG prefix.
func canonicalSignalName(name string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "SIG")
}

// parseSignal resolves a signal name (case-insensitive, with or without the SIG
// prefix) to its syscall.Signal.
func parseSignal(name string) (syscall.Signal, error) {
	if sig, ok := signalNames[canonicalSignalName(name)]; ok {
		return sig, nil
	}
	names := make([]string, 0, len(signalNames))
	for n := range signalNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return 0, fmt.Errorf("unsupported signal %q (supported: %s)", name, strings.Join(names, ", "))
}

// rollback restores file snapshots and then runs the rollback command, if any.
// It reports the outcome to stderr and returns a non-nil error if the rollback
// did not fully succeed (the system may be in an inconsistent state).
func rollback(snaps []fileSnapshot, rollbackCommand string) error {
	var errs []error
	if err := restoreSnapshots(snaps); err != nil {
		fmt.Fprintf(os.Stderr, "c-txn: failed to restore snapshots: %v\n", err)
		errs = append(errs, err)
	}
	if rollbackCommand != "" {
		if err := runShell(rollbackCommand); err != nil {
			fmt.Fprintf(os.Stderr, "c-txn: rollback command exited with error: %v\n", err)
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "c-txn: rollback FAILED; the system may be in an inconsistent state")
		return errors.Join(errs...)
	}
	fmt.Fprintln(os.Stderr, "c-txn: rollback completed")
	return nil
}

// exitCode extracts the process exit code from a command error, defaulting to 1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ee.ExitCode()
	}
	return 1
}

// waitForSignal reports whether a SIGUSR1 arrives on sigCh before the deadline.
func waitForSignal(sigCh <-chan os.Signal, deadline time.Time) bool {
	// A signal may already be buffered (e.g. delivered during command
	// execution); give it priority over an already-elapsed deadline.
	select {
	case <-sigCh:
		return true
	default:
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case <-sigCh:
		return true
	case <-timer.C:
		return false
	}
}

// runShell runs a command string via "sh -c", wiring through the standard streams.
func runShell(command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// takeSnapshots backs up each path into dir, recording whether it existed.
func takeSnapshots(paths []string, dir string) ([]fileSnapshot, error) {
	snaps := make([]fileSnapshot, 0, len(paths))
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}

		info, err := os.Stat(abs)
		if errors.Is(err, os.ErrNotExist) {
			snaps = append(snaps, fileSnapshot{path: abs, existed: false})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s: is a directory (only regular files are supported)", p)
		}

		backup := filepath.Join(dir, fmt.Sprintf("%d-%s", i, filepath.Base(abs)))
		if err := copyFile(abs, backup, info.Mode()); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		snaps = append(snaps, fileSnapshot{path: abs, existed: true, mode: info.Mode(), backup: backup})
	}
	return snaps, nil
}

// restoreSnapshots restores files to their snapshot state. Files that existed are
// rewritten from their backup; files that did not exist are removed.
func restoreSnapshots(snaps []fileSnapshot) error {
	var errs []error
	for _, s := range snaps {
		if s.existed {
			if err := copyFile(s.backup, s.path, s.mode); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.path, err))
			}
			continue
		}
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s: %w", s.path, err))
		}
	}
	return errors.Join(errs...)
}

// copyFile copies src to dst, creating or truncating dst with the given mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
