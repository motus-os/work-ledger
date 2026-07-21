# Motus Work Ledger

Motus records command runs in a local SQLite ledger. It keeps the command name,
timing, Git state, output counts, exit status, and outcome without storing the
command's arguments or raw output.

Terminal scrollback disappears and CI logs live in separate systems. Motus
gives those runs one local index and a deterministic JSON receipt you can read
later.

## Try it from source

Motus has not reached its first public release. Build the current source with
Go 1.26.5:

```console
$ go build -o ./bin/motus ./cmd/motus
$ ./bin/motus wrap -- go test ./...
ok      github.com/example/project  0.42s
motus: recorded run_7e2f... (success)
Next: ./bin/motus --state-dir /work/project/.motus run receipt run_7e2f...
```

Then list and inspect the record:

```console
$ ./bin/motus run list
RUN ID      STATE   OUTCOME  STARTED (UTC)         COMMAND
run_7e2f... closed  success  2026-07-21T17:27:33Z  go

$ ./bin/motus run receipt run_7e2f... > receipt.json
$ ./bin/motus doctor
PASS  state path: state directory and database paths pass local safety checks
...
Scope: local consistency only; producer-controlled records are not independently authenticated.
```

`motus wrap` starts the command directly, without a shell. It forwards stdin
and copies stdout and stderr to their original destinations. Motus records byte
and newline counts after the streams are drained. Because output is copied
through pipes, a program that checks for a terminal can format its output
differently than it would when run directly.

## What is recorded

- a random run ID and timestamps
- the executable's base name and argument count
- the Git repository name, commit, and pre-run dirty state when available
- stdout and stderr byte and newline counts
- exit code, terminating signal, and outcome
- canonical event payloads created by Motus

Motus does not store command argument values, stdin, raw stdout or stderr,
environment variables, source files, prompts, or agent transcripts. It does
not send ledger data over the network.

## State and commands

By default, Motus uses `.motus/ledger.db` at the current Git root. Outside a
Git repository, it uses the current directory. Override this with
`--state-dir PATH` or `MOTUS_STATE_DIR`.

```text
motus wrap -- COMMAND [ARG ...]  Run a command and record selected metadata
motus run list [OPTIONS]         List and filter recorded runs
motus run receipt RUN_ID         Write a JSON receipt for a closed run
motus doctor [--json]            Check local database consistency
motus version                    Print version information
```

Add `.motus/` to the project's ignore file if the ledger should remain outside
version control.

## Trust boundary

A Motus receipt is a producer-controlled process record. Database triggers
block ordinary updates and deletes, and `motus doctor` checks the current
schema, hashes, sequences, foreign keys, and terminal records. A person who
controls the database file can replace those controls and rewrite a
self-consistent history. Motus does not sign records, provide an independent
observation, or prove that the recorded work was correct.

The receipt states this boundary as `"trust_model":"producer-controlled"`.
See [SECURITY.md](SECURITY.md) for the full security model and
[ARCHITECTURE.md](ARCHITECTURE.md) for the data flow and storage design.

## Development

The normal checks use standard Go commands:

```console
$ go test -race ./...
$ go vet ./...
$ go build ./cmd/motus
```

The default pull-request workflow runs once on Ubuntu. Release builds run the
suite natively on Ubuntu, macOS, and Windows first; the same native check can
also be started manually.

See [CONTRIBUTING.md](CONTRIBUTING.md) before proposing a change.

## License

Apache License 2.0. See [LICENSE](LICENSE). Binary archives also include the
applicable [third-party notices](third_party_licenses/README.md).
