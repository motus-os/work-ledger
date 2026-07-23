# Motus Work Ledger

Motus records command runs in a local SQLite ledger. It keeps the executable
name, timing, Git context, output counts, exit status, and outcome without
storing argument values or raw output. When a run exposes something worth
keeping, you can connect a short finding to it and later close that finding
with a successful run.

Terminal scrollback disappears and CI logs live in separate systems. Motus
gives those runs one local index, searchable findings, and deterministic JSON
receipts for closed runs.

## Try it from source

Build the current source with Go 1.26.5:

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

If a failed wrapped run exposed something worth keeping, record the finding
through standard input. The text is read by Motus, not parsed as a command-line
value. In this example, `run_failed...` is the ID printed by the failed run.
Type one line, then press Ctrl-D to finish:

```console
$ ./bin/motus finding add --run run_failed... --file -
The retry reused stale generated state.
Recorded finding_91ac... (open)
Run: run_failed...
Summary: The retry reused stale generated state.

$ ./bin/motus finding list --state open
FINDING ID       STATE  RECORDED (UTC)         ORIGIN RUN   SUMMARY
finding_91ac...  open   2026-07-21T17:31:05Z  run_failed... The retry reused stale generated state.
```

Run the fix through `motus wrap`. If it succeeds, close the finding with that
run's ID:

```console
$ ./bin/motus finding close finding_91ac... \
    --disposition resolved --run run_fixed... --file -
Regenerated the file before the test.
(press Ctrl-D)
Closed finding_91ac... (resolved)
Resolving run: run_fixed...
```

Motus keeps the original finding and appends a separate closure instead of
rewriting it.

## What is recorded

- a random run ID and timestamps
- the executable's base name and argument count
- the Git repository name, commit, and pre-run dirty state when available
- stdout and stderr byte and newline counts
- exit code or terminating signal when available, and outcome
- canonical event payloads created by Motus
- finding summaries, hypotheses, next steps, and closure notes that you
  explicitly supply

Motus does not store command argument values, command stdin, raw stdout or
stderr, environment variables, source files, prompts, or agent transcripts.
Finding text is the exception: Motus stores the validated finding content you
submit through a file or stdin. It does not send ledger data over the network.

## State and commands

By default, Motus uses `.motus/ledger.db` at the current Git root. Outside a
Git repository, it uses the current directory. Override this with
`--state-dir PATH` or `MOTUS_STATE_DIR`.

Motus creates state directories with private POSIX permissions. If you create
a custom state directory yourself, restrict it to the current user before use.

```text
motus wrap -- COMMAND [ARG ...]  Run a command and record selected metadata
motus run list [OPTIONS]         List and filter recorded runs
motus run receipt RUN_ID         Write a JSON receipt for a closed run
motus finding add [OPTIONS]      Connect an authored finding to a run
motus finding list [OPTIONS]     List and search findings
motus finding show FINDING_ID    Show a finding and its run context
motus finding close [OPTIONS]    Resolve or dismiss a finding
motus doctor [--json]            Check local database consistency
motus version                    Print version information
```

Add `.motus/` to the project's ignore file if the ledger should remain outside
version control.

### Wrapped command behavior

`motus wrap` starts the command directly, without a shell. It forwards stdin
and copies stdout and stderr to their original destinations. Motus records byte
and newline counts observed before the command finishes or Motus terminates it.
Because output is copied through pipes, a program that checks for a terminal
can format its output differently than it would when run directly. If an
output destination closes, Motus stops the command tree and records a failure.

## Trust boundary

A Motus receipt is a producer-controlled process record. Findings are also
producer-controlled records: their text is authored, not independently
verified or inferred by Motus. Database triggers block ordinary updates and
deletes, and `motus doctor` checks the current schema, hashes, sequences,
foreign keys, terminal records, and finding links. A person who controls the
database file can replace those controls and rewrite a self-consistent history.
Motus does not sign records, provide an independent observation, or prove that
the recorded work was correct.

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
