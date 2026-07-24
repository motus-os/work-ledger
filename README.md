# Motus Work Ledger

Logs show what happened. Motus keeps what you choose to save from the work.

Motus records selected facts about a command run. After a failure, you can add
a summary, likely cause, and next step, then link that finding to a later
successful run. Search the ledger when the problem returns.

Git shows what changed. Shell history and CI show what ran. Motus gives the
failed run, your explanation, and the successful run stable IDs in one local
SQLite database. Closed runs also have repeatable JSON receipts.

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

Motus stores this project's ledger at `.motus/ledger.db`. Add `.motus/` to
`.gitignore` before recording work. Finding text is content you submit, so
review it before saving.

## Record a failure and what worked next

This example uses `npm test`. Replace it with any test, build, script, or tool
you already run. A run is one command execution recorded by Motus. A finding
is a short note about a failure: what happened, the likely cause, or the next
step.

Every run gets a `run_` ID. Every finding gets a `finding_` ID. Those IDs
connect the failed run, your note, and the successful run after the fix. The
examples below use shortened IDs.

### 1. Record the failed command

```console
$ motus wrap -- npm test
motus: recorded run_0370... (failure)
Keep why this failed:
  motus --state-dir /work/project/.motus finding add --run run_0370... --file -

Inspect the run:
  motus --state-dir /work/project/.motus run receipt run_0370...
```

Motus shows the command's output normally and returns its exit status. After a
failure, it prints the exact command for adding a finding.

### 2. Add a finding

Copy the printed command. Finding text is read from a file or standard input,
not from a command-line argument. Type one short note, then press Ctrl-D on
macOS or Linux. On Windows, press Ctrl-Z and then Enter.

```console
$ motus finding add --run run_0370... --file -
The generated file was stale.
Recorded finding_1457... (open)
Run: run_0370...
Summary: The generated file was stale.
```

The plain-text form records one summary. When another operator or agent may
need the reasoning later, use JSON to include the likely cause and next step:

```console
$ motus finding add --run run_0370... --format json --file -
{
  "summary": "The generated file was stale.",
  "hypothesis": "Generation did not run before the test.",
  "next_step": "Regenerate the file, then rerun the test."
}
Recorded finding_1457... (open)
Run: run_0370...
Summary: The generated file was stale.
```

### 3. Record the successful check

Fix the problem, then run the same command through Motus. Keep the new run ID
if it succeeds.

```console
$ motus wrap -- npm test
motus: recorded run_c588... (success)
```

### 4. Link the finding to the successful run

Use the finding ID from step 2 and the successful run ID from step 3. Enter a
short closure note and finish input with the same EOF key sequence.

```console
$ motus finding close finding_1457... --disposition resolved --run run_c588... --file -
Refreshed the generated file before running the check.
Closed finding_1457... (resolved)
Resolving run: run_c588...
```

Motus leaves the original finding unchanged and adds a separate closure.

### 5. Search when the problem returns

```console
$ motus finding list --query stale
FINDING ID       STATE     RECORDED (UTC)         ORIGIN RUN   SUMMARY
finding_1457...  resolved  2026-07-24T18:51:48Z  run_0370...  The generated file was stale.

$ motus finding show finding_1457...
```

The full finding shows the authored summary, likely cause, next step, closure
note, and links to both runs.

## Use Motus with an agent

Any agent that can run project commands can use Motus. Add this to the
project's `AGENTS.md` or equivalent:

```markdown
## Motus

- Search open and resolved Motus findings before work where an earlier failure
  may help.
- Run meaningful tests, builds, scripts, and release checks through
  `motus wrap`.
- Record a finding only when the work produced guidance worth reusing.
- Resolve a finding only with a successful recorded run that addresses it.
```

The agent or operator chooses what deserves a durable record. Motus does not
inspect a conversation or infer a lesson.

## Data Motus keeps

- a random run ID and timestamps
- the executable's base name and argument count
- the Git repository name, commit, and pre-run dirty state when available
- stdout and stderr byte and newline counts
- exit code or terminating signal when available, and outcome
- structured run events created by Motus
- finding summaries, likely causes, next steps, and closure notes that you
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
motus wrap -- COMMAND [ARG ...]  Run a command and record selected facts
motus run list [OPTIONS]         List and filter recorded runs
motus run receipt RUN_ID         Write a JSON receipt for a closed run
motus finding add [OPTIONS]      Add a finding to a closed run
motus finding list [OPTIONS]     List and search findings
motus finding show FINDING_ID    Show a finding and its run context
motus finding close [OPTIONS]    Resolve or dismiss a finding
motus doctor [--json]            Check local ledger consistency
motus version                    Print version information
```

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

A receipt reports what the local ledger says about a run. Finding text comes
from whoever submits it; Motus does not infer or verify it. SQLite rules block
normal changes, and `motus doctor` checks for inconsistencies, but anyone who
controls the database file can rewrite it.

Motus does not sign records or observe the command independently. Receipts
identify this boundary as `"trust_model":"producer-controlled"`; they do not
prove that the work was correct. See [SECURITY.md](SECURITY.md) for the full
security model and [ARCHITECTURE.md](ARCHITECTURE.md) for the data flow and
storage design.

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
