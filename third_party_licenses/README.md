# Third-party licenses

These notices cover the Go modules linked into Motus Work Ledger by the
versions recorded in `go.mod` and `go.sum`. Release archives include this
directory with the application license.

| Module | Version | Upstream license file |
| --- | --- | --- |
| `github.com/ncruces/go-sqlite3` | `v0.35.2` | `github.com/ncruces/go-sqlite3/LICENSE` |
| `github.com/ncruces/go-sqlite3-wasm/v3` | `v3.2.35303` | `github.com/ncruces/go-sqlite3-wasm-v3/LICENSE` and `LICENSE-PARSER` |
| `github.com/ncruces/julianday` | `v1.0.0` | `github.com/ncruces/julianday/LICENSE` |
| `golang.org/x/sys` | `v0.46.0` | `golang.org/x/sys/LICENSE` |

When dependencies change, regenerate the module list, review each upstream
license, and update these files before release. The notices are not a substitute
for the terms in the upstream projects.
