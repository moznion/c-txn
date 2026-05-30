# c-txn

A transactional command runner.

`c-txn` runs a command and then waits for a confirmation signal (`SIGUSR1` by
default) within a timeout. If the signal arrives in time, the "transaction" is
**committed** and the process exits `0`. If the timeout elapses without the
signal, the transaction is **rolled back**: any file snapshots taken before the
command ran are restored, the rollback command (if any) is executed, and the
process exits with a non-zero status.

This is useful for "commit-or-revert" workflows — for example, applying a risky
configuration change and only keeping it if an external health check confirms
success by sending a signal back.

## Installation

```sh
go install github.com/moznion/c-txn@latest
```

Or build from source:

```sh
make build      # produces ./c-txn
```

## Usage

```sh
c-txn --timeout 30 \
      --command '<command to apply the change>' \
      --rollback-command '<command to undo the change>'
```

Take the snapshot of files so they can be restored on rollback:

```sh
c-txn --timeout 30 \
      --command 'deploy.sh' \
      --files /etc/app/config.yml,/etc/app/secrets.yml \
      --rollback-command 'systemctl reload app'
```

To confirm (commit) the transaction, send the configured signal to the `c-txn`
process before the timeout elapses:

```sh
kill -USR1 <pid-of-c-txn>
```

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--timeout` | _(required)_ | Seconds to wait for the confirmation signal before rolling back. Must be `> 0`. The budget covers the whole transaction, including the time spent running `--command`. |
| `--command` | _(none)_ | Command to run. Optional — when omitted, `c-txn` just waits for the signal. Executed via `sh -c`. |
| `--rollback-command` | _(none)_ | Command to run during rollback (after file snapshots are restored). |
| `--files` | _(none)_ | Files to snapshot before the command runs and restore on rollback. Repeatable and/or comma-separated. Regular files only. |
| `--rollback-on-failure` | `false` | Roll back when `--command` itself exits non-zero. By default a failing command exits immediately **without** rolling back. |
| `--signal` | `USR1` | Confirmation signal to wait for. Accepts `USR1`, `USR2`, `HUP`, `INT`, `TERM`, `QUIT`, case-insensitive, with or without the `SIG` prefix. |
| `--timeout-exit-code` | `100` | Exit code used when the timeout elapses without the confirmation signal. |
| `--rollback-failure-exit-code` | `101` | Exit code used when the rollback itself fails (snapshot restore or rollback command errored). |
| `--rollback-on-failure-exit-code` | `99` | Exit code used when `--command` fails and `--rollback-on-failure` rolls back successfully. |

### Behavior

1. The signal handler is registered first, so a signal delivered while the
   command is still running is not missed.
2. If `--files` is given, the listed files are snapshotted (their content and
   permissions; files that do not exist are recorded as absent).
3. `--command` runs (if provided).
4. `c-txn` waits for the confirmation signal until the deadline:
   - **Signal received** → commit, keep all changes, exit `0`.
   - **Timeout** → restore snapshots, run `--rollback-command`, then exit.
     - Restored files that existed are rewritten from their backup; files that
       did not exist at snapshot time are removed.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Committed — the confirmation signal was received in time. |
| `2` | Usage error (e.g. missing/invalid `--timeout`, unknown `--signal`). |
| _command's own code_ | `--command` failed and rollback was **not** requested (default). |
| `--rollback-on-failure-exit-code` (default `99`) | `--command` failed and `--rollback-on-failure` rolled back successfully. |
| `--timeout-exit-code` (default `100`) | Timed out without the signal; rolled back successfully. |
| `--rollback-failure-exit-code` (default `101`) | Rollback itself failed; the system may be in an inconsistent state. |

## Development

Common tasks are wired up in the `Makefile`:

```sh
make            # lint, lint-actions, test, then build
make build      # go build -o c-txn .
make test       # go test ./...
make lint       # golangci-lint run ./...
make lint-actions  # actionlint (validates GitHub Actions workflows)
make fmt        # go fmt ./...
make clean      # remove the built binary
```

Requires Go (see `go.mod` for the version), and `golangci-lint` / `actionlint`
for the lint targets.

## CI

GitHub Actions runs the test and lint jobs on every push to `main` and on pull
requests. See [`.github/workflows/ci.yml`](.github/workflows/ci.yml).
