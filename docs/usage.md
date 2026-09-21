# Using Work Ledger

For installation and a complete worked example, start with
[Get started](get-started.md). This page covers other findings, automation,
state management, and recovery.

## Authoring findings

A finding can be attached to any closed run, not just a failure. Preserve a
run-specific explanation, constraint, workaround, decision, or next step.
Leave it open while it remains useful. Resolve it when a successful recorded
run addresses it, or dismiss it with a note if it is wrong, stale, or no longer
needed.

![A finding stays linked to its origin run. It remains open until one closure either resolves it with a successful run or dismisses it with a note. The original finding does not change.](finding-lifecycle.svg)

Motus reads authored content from a file or stdin (`--file -`), never an inline
command argument. Review the text before submitting it. Input files and
exported records remain sensitive even if the ledger directory is ignored by
Git.

The default `text` format creates a single summary. Use JSON **instead of**
text when separate fields are useful:

```json
{
  "summary": "The generated file was missing.",
  "hypothesis": "Generation did not run before the test.",
  "next_step": "Generate the file, then rerun the test."
}
```

Save this as `finding.json`, replace `RUN_ID` with a closed run's full ID, and
run:

```sh
motus finding add --run RUN_ID --format json --file finding.json
```

This creates a new finding. It does not add fields to an existing finding.
Each invocation of `add` creates another record.

Input must be valid UTF-8. A summary is required, single-line, and at most 512
UTF-8 bytes. `hypothesis`, `next_step`, and a closure's `note` can each contain
up to 4096 bytes, including internal LF newlines. Do not include leading or
trailing whitespace, tabs, control characters, or bidirectional formatting.
The total input limit is 16 KiB. For text input, Motus removes one final LF or
CRLF before validation; it does not trim extra blank lines or spaces. In JSON,
use `\n` for internal newlines, not `\r\n`.

To dismiss an open finding, put the reason in a text file, then run:

```sh
motus finding close FINDING_ID --disposition dismissed --file reason.txt
```

To resolve it, use `--disposition resolved --run RUN_ID` with a closed,
successful run in the same ledger. The note states why that run resolves the
finding; Motus does not infer chronology or causation. A finding can be closed
once. The CLI has no edit, reopen, or delete command. Review content before
saving it; `doctor` does not undo an unwanted record.

## Search and inspect

```sh
motus finding list --query "generated file"
motus finding show FINDING_ID
motus finding show FINDING_ID --json
```

`list` returns matching findings. `show` includes the selected finding, its
closure when present, and its origin and resolving run records. JSON output
uses `finding`, `origin_run`, and, when present, `resolving_run`; the summary is
at `finding.content.summary`.

Search matches terms case-insensitively across the summary, hypothesis, next
step, and closure note. Full IDs and hexadecimal ID fragments of at least
eight characters are searchable. Results matching the complete query or more
terms appear first; ties are newest-first. Add `--state open`, `--state
resolved`, or `--state dismissed` to filter. `--limit`, `--offset`, and `--json`
support scripts and agents. Search reads only the selected ledger.

## Coding agents and CI

Put Motus calls in the workflow that already runs the command. A developer,
agent, or script chooses which runs and findings to keep. Project guidance
can be as simple as:

```markdown
## Motus

- Search relevant findings before related work.
- Use `motus wrap` for tests, builds, or checks whose result is worth keeping.
- Add a finding when there is context someone will need again.
- Explain a resolution in a closure note and link the successful run.
- Review authored content for secrets and private information before saving.
```

For automation, use `--json` rather than parsing human output. A caller-owned
script can use `finding show` to update a ticket or other record. Motus does
not route work or keep external systems synchronized. Keep standing rules in
project documentation and decisions in the systems that own them.

Use an explicit state directory in CI. An ephemeral runner loses the ledger
unless you preserve it. Stop all Motus activity before archiving the whole
state directory, and restore it before a later job searches it. Review authored
content before upload; CI artifacts have their own access and retention rules.
Never treat a confirmation failure as proof that an `add` or `close` did not
commit. [Check before retrying](#troubleshooting-and-recovery).

## State directories and worktrees

The default is `.motus/ledger.db` at the current Git root, or under the current
directory outside Git. Each clone and worktree therefore has its own default
ledger. `--state-dir PATH` overrides `MOTUS_STATE_DIR` and must precede the
subcommand:

```sh
motus --state-dir /absolute/path/to/ledger finding list
```

Replace that example path with the directory containing your `ledger.db`.
Relative paths resolve from the current working directory. Use an absolute
path when commands from different folders should consult the same ledger.

A list command against a missing ledger exits with status `1`. An existing
ledger with no matches exits with status `0`; JSON list output is `[]`.
This distinction helps detect a wrong path. An attempted write against a new
state path can create an empty ledger before rejecting an unknown run ID.

### Back up and restore

Stop every Motus process using the ledger. Copy the **entire state directory**,
not just `ledger.db`, to a private backup location. SQLite can keep journal
files beside the database. Preserve the private permissions: no broader than
`0700` on the directory and `0600` on the database on Unix. On Windows, check
the folder's access permissions.

For a default `.motus/` ledger on macOS or Linux, from the project directory,
use the following commands. Choose new archive and restore names if these
already exist. This block stops if either name is already present:

```sh
(
  set -eu
  umask 077
  test ! -e motus-state.tar
  mkdir -m 700 restored-state
  tar -cf motus-state.tar .motus
  tar -C restored-state -xf motus-state.tar
  motus --state-dir restored-state/.motus doctor
  motus --state-dir restored-state/.motus finding list
)
```

Keep the archive in a private location and do not commit it. On Windows, copy
the whole directory to a new private restore folder and run the same `doctor`
and `finding list` commands with that folder's path. `--state-dir` takes the
copied ledger directory, not its parent or the database filename.

Keep the original intact until the restored copy passes `doctor` and you can
open an expected finding with `finding show`. A backup does not merge ledgers.

## Troubleshooting and recovery

**A write's confirmation is missing.** A finding or closure may have been saved
before its output failed. Search and inspect the same ledger before retrying:

```sh
motus finding list --query "words from the summary"
motus finding show FINDING_ID
```

Use the same `--state-dir` as the write if it was explicit. Repeating `add`
creates a new finding; repeating `close` after a saved closure reports a
conflict. An exit error alone does not prove that nothing was written.

**The ledger is read-only.** A clean, current-version ledger can be inspected
without write access. An older WAL ledger or an interrupted transaction may
need a writable open first. Stop other ledger users, back up the whole state
directory, and run the current `motus --state-dir "PATH" doctor` with write
access. Replace `PATH` with that directory's absolute path, keeping the quotes.
Restore the intended read-only permissions after it succeeds.

Versions through v0.1.4 used WAL mode. Current Motus validates an isolated copy
before migrating the original to rollback-journal mode. Migration needs
exclusive write access. Do not open the migrated ledger with v0.1.4 or older;
they re-enable WAL. If that happens, stop other users and run current `doctor`
again.

**A run stayed open after a crash.** `doctor` checks consistency; it does not
close abandoned runs. An open run cannot produce a receipt or accept a finding.
Inspect it with `run list`. Rerun the underlying command only if doing so is
safe; a crash does not establish whether that command changed anything.

**Doctor fails.** Keep the original state and the error message. Do not edit
SQLite tables, delete journals, or replace the ledger to silence the check.
Use a verified backup or report the problem without attaching private ledger
contents. See [Security](../SECURITY.md) for sensitive reports.

## Run receipts

`motus run receipt RUN_ID` writes a deterministic, self-hashed JSON projection
of a closed run using `motus.work-receipt.v1`. Findings and closure notes are
not part of a receipt. Adding or closing a finding does not change the receipt
bytes for its linked run.

The receipt reports what the producer-controlled local ledger says. It is not
signed, independently observed, or proof that all relevant work was recorded.
GitHub artifact attestations verify release archives, not these records.
See [the security model](../SECURITY.md#trust-boundary).

## Command reference

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

Use `motus --help` or a subcommand's `--help` for its options.

### Wrapped commands

`wrap` starts the command directly, without a shell. Shell syntax such as
pipes or redirection requires an explicit shell command. On Windows, batch
commands such as `npm.cmd` also need the interpreter, for example
`motus wrap -- cmd /d /c npm test`. The `/d` option disables command-processor
AutoRun entries for this invocation. Motus forwards stdin
and copies stdout and stderr to their original destinations. Because the
program sees pipes rather than a terminal, formatting or interactive behavior
can differ from running it directly. If an output destination closes, Motus
stops the supervised command tree and records a failure.

Motus passes through the environment and operating-system capabilities of the
wrapped command. It is not a sandbox. See [Architecture](../ARCHITECTURE.md)
for process and storage behavior, and [Security](../SECURITY.md) for platform
limits and the recorded-data boundary.

[Back to the README](../README.md)
