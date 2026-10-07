# Contributing

Thanks for helping. agentyard is deliberately small: one stdlib-only Go binary and one embedded
`page.html`. Changes that keep it that way are easy to merge.

## Ground rules

- Go standard library only. No new dependencies, no JS build step, no frameworks.
- Every concept has one owner. If you find two places answering the same question, fix it in the same PR.
- UI state is said in content (icon, dot, label), never as a coloured left-border stripe.
- Never put real repos, PR titles or session transcripts in issues, screenshots or tests. Use
  `agentyard demo`, which serves synthetic data.

## Develop

```sh
go build -o agentyard .
./agentyard demo            # full UI on synthetic data, nothing touches git, gh or disk
gofmt -l .                  # must print nothing
go vet ./... && go test -race ./...
```

## Pull requests

Keep them focused, explain the why, and include a screenshot taken from `agentyard demo` for UI
changes. CI runs gofmt, vet, tests and a build on macOS and Linux.

## Releases

Maintainers tag `vX.Y.Z`; GitHub Actions runs GoReleaser, which publishes archives, checksums and the
Homebrew cask. Update `CHANGELOG.md` first.
