# Security

## Supported versions

Motus has not reached its first public release. Security fixes currently apply
to the latest commit on `main`.

## Report a vulnerability

Do not open a public issue for a suspected vulnerability. Use the repository's
[private vulnerability-reporting form](https://github.com/motus-os/work-ledger/security/advisories/new).

## Security model

Motus protects a local ledger from accidental mutation and ordinary writes
through its API:

- state paths reject final-component symlinks and broad POSIX permissions
- SQL uses parameters for record values
- event payloads must be bounded, valid UTF-8 JSON without duplicate keys
- payload SHA-256 values are recomputed during receipt projection and doctor
- SQLite constraints and triggers enforce lifecycle and append rules
- command arguments, stdin, and raw process output are not stored
- Motus has no telemetry or ledger network client

`motus doctor` reports current local consistency. It checks the expected schema
definitions, SQLite integrity, foreign keys, metadata, payload hashes, event
sequences, and terminal records.

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

The version-1 Store accepts only the generated `run.started` and
`run.terminal` lifecycle payloads. Any future integration that introduces new
event payloads must define and test its privacy boundary before the schema is
expanded.
