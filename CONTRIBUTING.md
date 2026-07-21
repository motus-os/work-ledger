# Contributing

Motus is intentionally small. Open an issue before proposing a new command,
schema field, dependency, or integration. Bug fixes should include a focused
regression test.

## Local checks

Use Go 1.26.5, then run:

```console
$ go mod verify
$ go test -race ./...
$ go vet ./...
$ go build ./cmd/motus
```

Run `gofmt` on changed Go files. Do not commit generated databases, test
artifacts, credentials, transcripts, or private planning material.

## Pull requests

Keep a pull request to one coherent change. Explain the user-visible behavior,
the failure mode addressed, and the commands that actually passed. Do not
describe producer-controlled receipts as proof, verification, independent
attestation, or compliance evidence.

Report suspected vulnerabilities through the private path in
[SECURITY.md](SECURITY.md), not a public issue.
