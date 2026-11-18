# Contributing to omnia-dialhome

Thank you for your interest in contributing. This document describes the
sign-off requirement, the development setup and the pull request process.

## Developer Certificate of Origin (DCO)

This project uses the Developer Certificate of Origin (DCO) to ensure that
contributors have the right to submit their contributions. By making a
contribution to this project, you certify that:

1. The contribution was created in whole or in part by you and you have the
   right to submit it under the open source license indicated in the file; or
2. The contribution is based upon previous work that, to the best of your
   knowledge, is covered under an appropriate open source license and you have
   the right under that license to submit that work with modifications,
   whether created in whole or in part by you, under the same open source
   license (unless you are permitted to submit under a different license), as
   indicated in the file; or
3. The contribution was provided directly to you by some other person who
   certified (1), (2) or (3) and you have not modified it.

### Signing your commits

Every commit needs a `Signed-off-by` trailer whose email matches the commit
author. Add the `-s` flag to your commit command:

```bash
git commit -s -m "feat: add the thing"
```

This adds the trailer to your commit message:

```text
Signed-off-by: Your Name <your.email@example.com>
```

CI's `dco` job rejects a pull request with any commit missing a matching
trailer. To fix the last commit:

```bash
git commit --amend -s --no-edit
```

To sign off several commits, run `git rebase --signoff <base>`.

## Submitting changes

1. Fork the repository and create a branch: `git checkout -b feat/your-feature`.
2. Make your change, with tests.
3. Run `make test lint`.
4. Commit with `git commit -s` and a conventional-commit message (`feat:`,
   `fix:`, `docs:`, `chore:`).
5. Open a pull request against `main`.

## Changing the wire contract

The channel (stream kinds, handshake, the config JSON) and
`proto/omnia/runtime/v1/runtime.proto` are spoken by Omnia releases already
deployed. Never renumber a stream kind or a proto field. An additive proto
change bumps the minor `Contract-Version` in the proto and
`runtimev1.ContractVersion` together, then `make generate`.

## Development setup

- Go, at the version `go.mod` declares
- golangci-lint v2.14.0
- For `make generate`: `protoc`, `protoc-gen-go` and `protoc-gen-go-grpc`

```bash
make test   # go test -race -count=1 ./...
make lint
```
