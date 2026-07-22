# Architecture

Motus Work Ledger is one command-line program and one project-local SQLite
database. Version 1 has no server, account, plugin host, background process, or
telemetry client.

## Command flow

```text
motus wrap -- command args
          |
          +-- read Git metadata with output and time limits
          +-- create an open run
          +-- start the command directly, without a shell
          +-- forward stdin and copy stdout and stderr through pipes
          +-- count bytes and newline delimiters while streams are copied
          +-- close the run and append its terminal event in one transaction
          +-- print the run ID and receipt command
```

The command arguments and stdin are passed to the process but are not copied
into a Motus record. Raw output is forwarded and counted, not buffered in the
ledger. A program can detect that its stdout and stderr are pipes, so formatting
can differ from a direct invocation.

## Packages

- `cmd/motus` wires process signals and exit status to the CLI.
- `internal/cli` owns command parsing, output, and the record lifecycle.
- `internal/capture` supervises a process group or Windows Job Object, copies
  both output streams, and stops the tree if an output destination fails.
- `internal/gitmeta` reads repository root, commit, and dirty state with
  bounded output and a two-second deadline.
- `internal/store` owns the SQLite schema, transactions, projections, and
  consistency checks.

The packages are internal because the first release promises a command-line
interface and file format, not a stable Go library API.

## Store

The database is `.motus/ledger.db` unless the operator chooses another state
directory. New state directories and database files use private POSIX modes.
The application resolves symlinks in parent components and refuses a state
directory or database file that is itself a symlink. Windows network and
device paths are rejected.

SQLite runs with:

- WAL journal mode
- `synchronous=FULL`
- foreign keys and recursive triggers enabled
- `trusted_schema=OFF`
- one connection per Motus process
- immediate write transactions with bounded busy retries

The schema has three tables: append-enforced metadata, runs, and append-only
events. A run moves once from `open` to `closed`. Closing appends the final
`run.terminal` event and updates the run projection in the same transaction.
Database triggers reject event mutation, deletion, sequence gaps, writes after
the terminal event, run deletion, start-metadata mutation, and changes to a
closed run.

## Receipts

`motus run receipt RUN_ID` reads a run and its ordered events in one read
transaction. Projection stops if a payload hash, sequence, terminal position,
or terminal payload does not match the run projection.

The output schema is `motus.work-receipt.v1`. Its `receipt_sha256` is the
lowercase SHA-256 digest of the exact compact JSON bytes in the `receipt`
member. Projection has no current-time field, so unchanged stored data produces
the same receipt.

The hash detects an inconsistent receipt member. It is not a signature and
does not identify an independent observer.

## Failure behavior

Motus does not run a command if it cannot create the open record. After a
command starts, a downstream output failure terminates the supervised process
group or Job Object, waits for output forwarding to finish, then closes the run
as failed. Cancellation follows the same process-tree boundary and closes the
run as aborted.

An abrupt Motus crash can leave an open run. Committed events remain valid and
ordered. The first release does not guess how an interrupted run should be
closed.

## Deliberate boundaries

The first release does not include:

- transcript or raw-output capture
- a daemon or automatic observation
- signing, timestamping, or third-party attestation
- workflow orchestration
- organizational learning or recommendations

Those capabilities require separate evidence and contracts. They are not
implied by the ledger foundation.
