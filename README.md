# Motus Work Ledger

Motus is a local CLI for command runs and the findings you add to them. Use it
to keep an explanation, workaround, constraint, or decision connected to the
run that gave it context. Search that record when related work comes up again.

<picture>
  <source media="(max-width: 900px)" srcset="docs/motus-workflow-mobile.svg">
  <img src="docs/motus-workflow.svg" alt="A developer, agent, or CI job records a command run. A person or agent adds a finding linked to it. Later, list finds the finding and show opens its linked run context.">
</picture>

A **run** records selected facts: the executable, Git state when available,
timing, and outcome. A **finding** holds the context a person or agent writes.
A **closure** explains why the finding was resolved or dismissed. Resolving a
finding links it to a successful recorded run; the original finding stays intact.

The released CLI runs locally on macOS, Linux, and Windows. There is no account,
server, or background service. It does not collect activity automatically or
write findings for you.

## Install

**[Install Motus and try the first workflow](docs/get-started.md).** The guide
covers choosing and checking a v0.1.6 release archive, running the CLI, adding a
finding, and retrieving it later. You do not need Go to use a release binary.

Already installed? Run `motus version`, then go to
[the first workflow](docs/get-started.md#try-the-first-workflow).

## Record a run and add a finding

Start with a test, build, script, or check you already use:

```sh
motus wrap -- YOUR_COMMAND
```

Replace `YOUR_COMMAND` with the executable and arguments, for example
`npm test` on macOS/Linux or `cmd /d /c npm test` on Windows. Windows batch
commands such as `npm.cmd` need the command interpreter. Motus returns the
command's exit status and prints the recorded run ID. It forwards the command's
output without storing the raw text.

When there is something worth keeping, write a finding and link it to that run.
After a follow-up check, add a closure explaining what changed. Later, search
the same ledger and open the finding to see its context. The
[worked example](docs/get-started.md#try-the-first-workflow) includes the exact
commands and expected results, without requiring an existing project.

In a Git project, add `.motus/` to `.gitignore` before recording real work.
Review finding and closure text before saving it. Any separate input files or
exports also need an appropriate private home.

## Use it in your workflow

- [Findings, search, and command reference](docs/usage.md)
- [Coding agents and CI](docs/usage.md#coding-agents-and-ci)
- [State directories, worktrees, and backups](docs/usage.md#state-directories-and-worktrees)
- [Troubleshooting and recovery](docs/usage.md#troubleshooting-and-recovery)
- [Upgrade or uninstall](docs/get-started.md#upgrade-or-uninstall)

Keep standing rules in project documentation and decisions in the systems that
own them. Use a Motus finding when the source run, its result, or its later
resolution will help someone understand the work.

## Data and trust

Run records include selected metadata and output counts, not command argument
values, stdin, raw stdout or stderr, environment variables, source files,
prompts, or transcripts. Findings and closure notes contain the text you submit.
Motus has no ledger network client.

The local ledger is producer-controlled. Its checks can detect inconsistent
records, but the database owner can rewrite it. Local run receipts are not
signed and do not establish that the work was correct. GitHub release
attestations concern the downloaded software, not the work it records.

See [Security](SECURITY.md) for the trust and privacy boundaries and
[Architecture](ARCHITECTURE.md) for the record contract and storage design.
The [Motus vision](https://www.motussupra.com/vision.html) describes the broader
direction beyond this released local CLI.

## Development

With Go 1.26.5:

```sh
go mod verify
go test -race ./...
go vet ./...
go build ./cmd/motus
```

Pull requests run the Go quality checks on Ubuntu. The release workflow also
runs native checks on macOS and Windows. See [Contributing](CONTRIBUTING.md)
before proposing a change. Report suspected vulnerabilities through the private
path in [Security](SECURITY.md), not a public issue.

## License

Apache License 2.0. See [LICENSE](LICENSE). Binary archives also include the
applicable [third-party notices](third_party_licenses/README.md).
