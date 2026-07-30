# Security

## Supported versions

Only the latest published release is supported. Security fixes are developed
on `main` and included in the next release.

## Report a vulnerability

Do not open a public issue for a suspected vulnerability. Use the repository's
[private vulnerability-reporting form](https://github.com/motus-os/work-ledger/security/advisories/new).

## Security model

Motus guards the local ledger against accidental changes and normal
application writes:

- state paths reject final-component symlinks and broad POSIX permissions
- SQL uses parameters for record values
- event and finding payloads must be bounded, valid UTF-8 JSON without
  duplicate keys
- payload SHA-256 values are recomputed during receipt projection and doctor
- SQLite constraints and triggers enforce lifecycle and append rules
- wrapped-command arguments, stdin, and raw process output are not stored
- Motus has no telemetry or ledger network client

`motus doctor` reports current local consistency. It checks the expected schema
definitions, SQLite integrity, foreign keys, metadata, payload hashes, event
sequences, terminal records, and finding links.

Release archives include GitHub artifact attestations. They can verify that an
archive was built by this repository's release workflow. They do not attest to
records or receipts created by a local Motus ledger.

## Trust boundary

The database owner is outside the tamper-resistance boundary. An owner can drop
the SQLite triggers, change records and hashes, and restore a self-consistent
schema. A passing doctor report cannot prove that this never happened.

Receipts are not signed and Motus does not independently observe the command.
A receipt cannot prove that every relevant event was captured, that the
producer told the truth, or that the work was correct.

On Unix, cancellation targets the command's process group. A descendant that
deliberately creates a new session or process group can escape that portable
boundary. On Windows, Motus creates the command suspended and assigns it to a
Job Object before it runs. Native platform validation is required before each
release.

## Sensitive data

Motus creates run records without command arguments, stdin, raw output,
environment variables, source files, prompts, or transcripts. Motus stores the
finding summary, likely cause, next step, or closure note that a person or tool
submits through `--file` or stdin. Review that content before recording it.

Motus rejects control and bidirectional formatting characters in finding text
and never accepts finding or closure payloads as command-line values. File
paths and `--query` search terms remain visible as command-line values. These
controls reduce accidental exposure and unsafe terminal rendering; they do not
make submitted content non-sensitive.

SQLite may temporarily create `ledger.db-journal` beside `ledger.db` during a
write. Back up, move, or remove the entire state directory as one unit while no
Motus process is using it. Copies and backups remain readable to anyone who
can access them. If a process dies during a write, SQLite requires write access
to roll back a hot journal before the ledger can be opened read-only again.

Ledgers last opened by versions through v0.1.4 use WAL mode. A current Motus
binary needs a writable open to migrate such a ledger before read commands can
use it from a non-writable directory. Stop every Motus process using the
ledger, make the directory and database writable, run
`motus --state-dir PATH doctor`, and then restore the intended read-only
permissions. Motus validates an isolated copy of the exact ledger schema before
opening the source writable or changing its persistent journal mode.

Do not use v0.1.4 or older on that ledger after migration. Those versions
re-enable WAL on a writable open. If that happens, stop every Motus process and
run the current `doctor` command again.
