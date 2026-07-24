# Motus Work Ledger

Motus keeps the fix with the failure. It records selected facts about a
command run, lets you add the reason the failure mattered, and connects that
finding to the successful run that resolved it.

Git records source changes, not whether a selected command ran or how it
ended. Terminal scrollback disappears and CI logs live elsewhere. Motus gives
selected runs stable IDs, searchable findings, and deterministic JSON receipts
for closed runs, all in a local SQLite ledger.

## Install

With Go 1.26.5 or newer:

```console
$ go install github.com/motus-os/work-ledger/cmd/motus@latest
```

Go places the binary in `GOBIN`, or in `GOPATH/bin` when `GOBIN` is unset.
Make sure that directory is on your `PATH`.

Prebuilt archives are available from the
[latest release](https://github.com/motus-os/work-ledger/releases/latest).
Each release includes SHA-256 checksums, SBOMs, and GitHub artifact
attestations. Verify the archive, unpack it, and place `motus` or `motus.exe`
on your `PATH`.

<details>
<summary>Verify a release archive</summary>

Replace `ARCHIVE_NAME` with the exact downloaded filename.
Attestation verification uses the
[GitHub CLI](https://cli.github.com/).

On macOS:

```console
$ grep "  ARCHIVE_NAME$" checksums.txt | shasum -a 256 -c -
$ gh attestation verify ARCHIVE_NAME --repo motus-os/work-ledger
```

On Linux:

```console
$ grep "  ARCHIVE_NAME$" checksums.txt | sha256sum -c -
$ gh attestation verify ARCHIVE_NAME --repo motus-os/work-ledger
```

On Windows PowerShell:

```powershell
$archive = "ARCHIVE_NAME"
$expected = (Get-Content checksums.txt | Where-Object { $_ -like "*  $archive" }).Split()[0]
$actual = (Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actual -ne $expected) { throw "checksum mismatch" }
gh attestation verify $archive --repo motus-os/work-ledger
```

The macOS binaries are not Apple-notarized. After the checksum and GitHub
attestation pass, remove a browser-added quarantine flag if Gatekeeper blocks
the extracted binary:

```console
$ xattr -d com.apple.quarantine /path/to/motus
```

</details>

Confirm the installed command:

```console
$ motus version
```

## Keep a failure and its fix

Run a test, build, script, or tool through Motus. The wrapped command still
writes to the terminal and returns its own exit status. The transcript below
is condensed, uses shortened output IDs, and uses `YOUR_COMMAND` as a
placeholder for the command you want to run. Uppercase values are placeholders.

```console
$ motus wrap -- YOUR_COMMAND
motus: recorded run_0d27... (failure)
Keep why this failed:
  motus --state-dir /work/project/.motus finding add --run run_0d27... --file -

Inspect the run:
  motus --state-dir /work/project/.motus run receipt run_0d27...
```

Use the printed command to keep the explanation. Finding text is read from a
file or standard input, not from a command-line argument. Type one line, then
press Ctrl-D on macOS or Linux. On Windows, press Ctrl-Z and then Enter.

```console
$ motus finding add --run FAILED_RUN_ID --file -
The generated file was stale.
Recorded finding_c896... (open)
Run: run_0d27...
Summary: The generated file was stale.
```

Make the change, then run the same check through Motus. After it succeeds,
connect the finding to that run. Enter the closure note through standard input
and finish it with the same EOF key sequence.

```console
$ motus wrap -- YOUR_COMMAND
motus: recorded run_c0e5... (success)

$ motus finding close FINDING_ID --disposition resolved --run SUCCESS_RUN_ID --file -
Refreshed the generated file before running the check.
Closed finding_c896... (resolved)
Resolving run: run_c0e5...
```

Motus keeps the original finding and appends a separate closure instead of
rewriting it. When the problem returns, search your own words and inspect both
runs:

```console
$ motus finding list --query stale
FINDING ID       STATE     RECORDED (UTC)         ORIGIN RUN   SUMMARY
finding_c896...  resolved  2026-07-24T14:32:11Z  run_0d27...  The generated file was stale.

$ motus finding show finding_c896...
```

## What is recorded

- a random run ID and timestamps
- the executable's base name and argument count
- the Git repository name, commit, and pre-run dirty state when available
- stdout and stderr byte and newline counts
- exit code or terminating signal when available, and outcome
- structured run events created by Motus
- finding summaries, hypotheses, next steps, and closure notes that you
  explicitly supply

Motus does not store command argument values, wrapped-command stdin, raw stdout
or stderr, environment variables, source files, prompts, or agent transcripts.
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
The wrapped process keeps the same environment and operating-system
capabilities it would have when run directly; Motus is not a sandbox.

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
