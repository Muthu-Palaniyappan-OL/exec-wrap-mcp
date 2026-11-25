# Contributing

Thanks for helping out.

## Setup

```sh
go build ./...
go vet ./...
go test -race ./...
```

Go 1.25 or newer. The end-to-end tests in `e2e/` build the real binary and talk to it with the MCP client from the Go SDK, so they exercise the same path as an AI client.

## Adding support for a new program

Most programs need only a preset in `internal/session/presets.go`: a name, a prompt regexp anchored with `\z`, and an entry in `InferPreset`. Add a case to `TestPromptRegexes` in `internal/session/clean_test.go`. If you can, add an end-to-end test too (skip it when the program is not installed).

## Pull requests

- Keep changes focused; run `gofmt`.
- Use [Conventional Commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `docs:`, ...). Release notes are generated from them.
- CI must pass on Linux, macOS and Windows.
