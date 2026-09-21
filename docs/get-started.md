# Get started with Motus

Install the CLI, record a small check, add a finding, and retrieve it later.
This guide uses Work Ledger v0.1.6. The example needs a terminal and a plain-text
editor, but no existing project, Git repository, or service.

## Install

### Download and check the archive

Open the [v0.1.6 release](https://github.com/motus-os/work-ledger/releases/tag/v0.1.6).
Create a new download folder for this installation. Under **Assets**, download
the matching archive and `checksums.txt` into that folder. Do not reuse an
existing installation folder: extraction can overwrite its files. Keep the
original archive until verification is complete.
Follow this guide for the walkthrough; the v0.1.6 archive contains earlier
documentation.

| Computer | Archive |
| --- | --- |
| macOS, Apple silicon | `motus_0.1.6_darwin_arm64.tar.gz` |
| macOS, Intel | `motus_0.1.6_darwin_amd64.tar.gz` |
| Linux, x86-64 | `motus_0.1.6_linux_amd64.tar.gz` |
| Linux, ARM64 | `motus_0.1.6_linux_arm64.tar.gz` |
| Windows, x64 | `motus_0.1.6_windows_amd64.zip` |
| Windows, ARM64 | `motus_0.1.6_windows_arm64.zip` |

On macOS, **About This Mac** identifies Apple silicon or Intel. On Linux,
`uname -m` returns `x86_64` or `aarch64` for these targets. On Windows, look at
**Settings > System > About > System type**.

In a terminal, change to the download folder. Replace `ARCHIVE_NAME` below with
the filename from the table. Compute its SHA-256:

```sh
# macOS
shasum -a 256 ARCHIVE_NAME

# Linux
sha256sum ARCHIVE_NAME
```

```powershell
# Windows PowerShell
Get-FileHash ARCHIVE_NAME -Algorithm SHA256
```

Open `checksums.txt` in a text editor. The hash must match the entry for that
exact filename, ignoring letter case. If it differs, stop. Do not extract or
run that download. Check that both files came from the same release and
download them again.

The checksum checks that the download matches the published archive. If you
need build provenance, also [verify its GitHub attestation](#verify-build-provenance)
before extraction. Neither check replaces your organization's software policy.

### Extract and run

On macOS or Linux, extract the verified archive:

```sh
tar -xzf ARCHIVE_NAME
```

On Windows, right-click the verified ZIP and choose **Extract All**. Inside the
extracted `motus_0.1.6_OS_ARCH` folder are the executable, documentation, and
license notices. Create a `Motus` folder in your user home directory and move
the extracted folder into it, keeping its versioned name. For example, on an
Apple-silicon Mac the executable would be at
`$HOME/Motus/motus_0.1.6_darwin_arm64/motus`; on Windows x64 it would be at
`$HOME\Motus\motus_0.1.6_windows_amd64\motus.exe` in PowerShell. This does not
need administrator privileges. If that destination already exists, use a new
location. Do not replace an existing installation before checking the new one.

Open a terminal in the folder containing the executable, then run:

```sh
# macOS or Linux
./motus version
```

```powershell
# Windows PowerShell
.\motus.exe version
```

Expect version `0.1.6` and commit
`f16de909115d0a49346897d0d077e21ed86746ba`.

**macOS security warning:** this release is not Apple-notarized. If Gatekeeper
blocks it, follow your organization's policy and
[Apple's instructions for opening an app you trust](https://support.apple.com/en-us/102445).
After an attempted launch, an allowed exception may be available under
**System Settings > Privacy & Security > Open Anyway**. Do not disable
Gatekeeper or override a malware or damaged-app warning. Checksums and GitHub
attestations are not Apple notarization.

To use `motus` from other folders, add this executable's directory to the
current terminal's `PATH`. Run this while still in that directory:

```sh
# macOS or Linux
export PATH="$PWD:$PATH"
```

```powershell
# Windows PowerShell
$env:Path = "$($PWD.Path);$env:Path"
```

Now `motus version` should return the same version and commit. If it does not,
check `command -v motus` on macOS/Linux or `Get-Command motus` in PowerShell to
see which installation is selected.

For future terminals, add that directory's **absolute path** to your shell's
startup file, or to the Windows **user** Path environment variable. Do not put
the literal `$PWD` in a startup file: it would select whichever folder you
started in. Open a new terminal and run `motus version` to check the change.
You can also continue using the executable's full path without changing PATH.

## Try the first workflow

### Record a failed check

Leave the installation folder and choose a new, unused folder for this
example. The commands below start in your home directory and use `motus-demo`;
change that name if it already exists. Run each command separately and stop
if creating the folder fails:

```sh
cd "$HOME"
mkdir motus-demo
cd motus-demo
```

Keep all commands and text files below in this folder. The explicit
`--state-dir .motus` keeps this example's records here, even if another ledger
is configured on your machine.

The check asks whether `generated.txt` exists. It should fail on the first run.
Use the command for your operating system:

```sh
# macOS or Linux
motus --state-dir .motus wrap -- sh -c 'test -f generated.txt'
```

```powershell
# Windows PowerShell
motus --state-dir .motus wrap -- cmd /c "if exist generated.txt (exit /b 0) else (exit /b 1)"
```

Look for `motus: recorded run_... (failure)`. An exit status of `1` is expected:
the file is absent. Motus also prints follow-up commands with the full run ID.
Copy that ID, including `run_`; use it wherever this guide says `ORIGIN_RUN_ID`.

### Add the finding

Create a plain-text UTF-8 file named `finding.txt` in `motus-demo`, containing:

```text
The required file was missing. Generate it before running the check.
```

Use a single line, without spaces before or after the sentence or an extra
blank line. A final newline is fine. Check that your editor did not save it as
`finding.txt.txt` or as a rich-text document.

Replace `ORIGIN_RUN_ID` with the full ID from the failed check, then run:

```sh
motus --state-dir .motus finding add --run ORIGIN_RUN_ID --file finding.txt
```

Motus prints `Recorded finding_... (open)`. Keep that full ID as `FINDING_ID`
for the commands below. Motus recorded the run's outcome; you supplied the
explanation. It did not infer the finding from output.

### Record the follow-up and close the finding

Create `generated.txt` in `motus-demo` with any plain text. This stands in for
the output a generation step would create; the example check tests only that
the file exists.

Repeat the same `motus ... wrap` command you used above. This time, expect
`motus: recorded run_... (success)`. Keep this new full ID as `RESOLVING_RUN_ID`.

Create a UTF-8 plain-text file named `closure.txt` containing:

```text
Created the required file and reran the check successfully.
```

Replace both ID placeholders and run:

```sh
motus --state-dir .motus finding close FINDING_ID --disposition resolved --run RESOLVING_RUN_ID --file closure.txt
```

Expect `Closed finding_... (resolved)`. A resolved closure requires a successful
run in the same ledger. You decide whether that run addresses the finding and
explain the relationship in the note. Motus does not determine causation.

### Find it later

In a later terminal session, return to this `motus-demo` folder. If you did not
make the PATH change permanent, use the executable's full path in place of
`motus`. Search for the finding, then open the full ID returned by the search:

```sh
motus --state-dir .motus finding list --query "required file"
motus --state-dir .motus finding show FINDING_ID
```

The list should contain your resolved finding. The full view includes these
fields; IDs and dates below are omitted for readability:

```text
State: resolved
Origin run: ... (failure, ...)
Summary: The required file was missing. Generate it before running the check.
Resolving run: ... (success, ...)
Closure note: Created the required file and reran the check successfully.
```

`finding list` finds matches. `finding show` opens the finding, closure, and
linked runs. Add `--json` to `show` when a script or agent needs those records.

## Use your own workflow

Return to a real project and choose a check whose result will matter later.
Add `.motus/` to the project's `.gitignore` before recording work. Keep authored
input files private too; ignoring `.motus/` does not ignore a `finding.txt`
elsewhere in the project.

Replace the demo check with that command, for example `motus wrap -- npm test`
on macOS/Linux or `motus wrap -- cmd /d /c npm test` on Windows.
Write findings only when there is context worth keeping. They can come from
successful runs as well as failures. Search before related work, and keep
standing instructions in the documentation your team already uses.

Without an explicit state path, Motus uses the Git root's `.motus/` directory,
or the current directory outside Git. Each clone or worktree has its own
default ledger. See [state directories](usage.md#state-directories-and-worktrees)
before sharing records between environments.

## If something goes wrong

- **Command not found:** repeat the PATH check in the install section, or use
  the executable's full path. A temporary PATH change ends with that terminal.
- **No ledger found or an ID is missing:** return to the same example folder
  and use `--state-dir .motus` before the subcommand. Do not create new records
  just to make a lookup succeed.
- **Finding text rejected:** use plain UTF-8, a single summary line, and no
  leading or trailing whitespace. The summary limit is 512 UTF-8 bytes, not
  512 characters. [Full input rules](usage.md#authoring-findings) cover JSON
  and longer notes.
- **Output lost or interrupted after adding or closing:** search and show the
  finding before retrying. A write can finish before its confirmation reaches
  you. Repeating `add` can create a duplicate.

For backups, interrupted writes, and older ledgers, see
[recovery](usage.md#troubleshooting-and-recovery). Do not delete `.motus/` as an
installation troubleshooting step.

## Upgrade or uninstall

### Upgrade an earlier installation to v0.1.6

1. Stop every Motus process using the ledger and
   [back up the entire state directory](usage.md#back-up-and-restore). Keep the
   earlier executable and its matching backup until the upgrade is checked.
2. Follow the [installation steps](#install) using a new download and
   installation folder. Check the new binary with `./motus version` or
   `.\motus.exe version` before changing PATH. A failed download, checksum, or
   launch is a reason to stop, not to replace the working installation.
3. From the new installation folder, run `./motus --state-dir "PATH" doctor`
   (PowerShell: `.\motus.exe --state-dir "PATH" doctor`) against the **restored
   copy**. Replace `PATH` with the copied ledger directory's absolute path,
   keeping the quotes so paths with spaces work.
   Search and open an expected finding with `finding list` and `finding show`
   using the same executable and state path. Do not use bare `motus` yet;
   PATH may still select the old installation.
4. If the copy passes, stop all ledger users again. If the original changed
   since the backup, make a fresh backup before running the same checks on
   the original. Ledgers last opened by v0.1.4 or earlier need write access for
   [migration](usage.md#troubleshooting-and-recovery). Change PATH to the new
   executable's directory, then open a new terminal and check `motus version`.

If a check fails, keep the original ledger, the backup, and the error message.
Do not retry with an older binary against a ledger the new version has opened.
To return to the earlier version, use a separate copy of its pre-upgrade backup;
records added after that backup will not be in the restored copy. Motus does
not merge ledgers or undo a migration.

### Uninstall without removing your records

For an archive installation, stop Motus, remove its versioned installation
directory from PATH, and move that directory to Trash or the Recycle Bin.
First keep or back up any ledgers inside it. For a source installation, remove
only the `motus` executable (`motus.exe` on Windows); keep the shared `GOBIN`
or `GOPATH/bin` directory and its PATH entry. Ledgers in project folders or
explicit state directories remain there; installation and ledger storage are
separate.

Open a new terminal and check `command -v motus` on macOS/Linux or
`Get-Command motus` in PowerShell. If a path remains, another installation is
still selected. Do not remove its parent directory without checking what else
it contains. Removing `.motus/` is a separate data-deletion decision, not a
required uninstall step.

## Other installation options

### Verify build provenance

For build provenance checks, install the [GitHub CLI](https://cli.github.com/)
and authenticate it with `gh auth login`. From the download folder, replace
`ARCHIVE_NAME` and run this before extraction:

```sh
gh attestation verify ARCHIVE_NAME --repo motus-os/work-ledger --source-ref refs/tags/v0.1.6 --signer-workflow motus-os/work-ledger/.github/workflows/release.yml
```

Require a successful verification. This checks the archive's attestation
against the named repository, release tag, and signing workflow. It does not
attest to the contents of ledgers you later create. Release assets also include
an SBOM for each archive and its third-party license notices.

### Install from source

With Go 1.26.5 or newer:

```sh
go install github.com/motus-os/work-ledger/cmd/motus@v0.1.6
```

Go places the executable in `GOBIN` if set, otherwise in `GOPATH/bin`; use
`go env GOBIN GOPATH` to locate it and add that binary directory to PATH.
`motus version` reports the module version for this installation, not the
packaged release's commit and build date. Use a verified archive when you need
packaged-release provenance.

[Back to the README](../README.md)
