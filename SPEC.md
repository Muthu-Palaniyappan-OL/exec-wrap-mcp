# exec-wrap-mcp: Spec (v0.1)

An MCP server (stdio only) that wraps long-lived interactive programs (bash, gdb, python, psql, ...) as persistent **sessions**. State survives across tool calls (`export ONE=1` then `echo $ONE` prints `1`). Many sessions can run at once.

## 1. Stack and constraints
- Go 1.25+, one static binary (`CGO_ENABLED=0`). Transport is stdio only; stdout carries MCP frames only, logs go to stderr.
- MCP: official SDK `github.com/modelcontextprotocol/go-sdk` (typed `mcp.AddTool`, so input/output JSON Schemas derive from Go structs). It is maintained with Google, so no alternative is needed.
- Unix PTY: `github.com/creack/pty`. Windows: ConPTY (`github.com/aymanbagabas/go-pty`). If ConPTY is unavailable (Windows < 1809 or the call fails), fall back to pipes with `tty:false`. Sessions can also request `tty:false` on any OS.
- Process cleanup: unix uses `setsid` and kills the process group. Windows uses a Job Object with `KILL_ON_JOB_CLOSE`. All sessions die when the server exits (stdin EOF, SIGINT, SIGTERM).

## 2. Session model
- ID: `s_` + 8 random hex chars. State: `running | exited`. An exited session keeps its exit code and unread output until `kill` or TTL.
- Each session has a reader goroutine feeding a ring buffer (4 MiB cap, oldest bytes dropped, `dropped_bytes` counted) plus a read cursor for "unread" output.
- Per-session mutex: a second `send`/`read` while one is waiting returns `busy`. Different sessions are fully concurrent.
- Limits (flags/env): `--max-sessions 16`, `--idle-ttl 30m` (reaped after no tool call; reaper sweeps every 30s), `--max-buffer 4194304`.

## 3. Tools
All tools return structured output plus a text rendering of the same data. Durations are in ms.

### `launch`
In: `command` (string, required, e.g. `/bin/bash`), `args` ([]string), `cwd`, `env` (map, merged over the inherited env), `preset` (`bash|zsh|sh|python|gdb|psql|node|cmd|powershell|none`, default: inferred from the basename), `prompt_regex` (overrides the preset), `tty` (bool, default true), `rows`/`cols` (default 40x200), `startup_wait_ms` (default 1000; waits for the first prompt/idle and returns the banner).
Out: `session_id`, `pid`, `output`, `done_reason`, `tty_mode` (`pty|conpty|pipe`), `prompt_regex` (effective).
No shell is involved: `command` + `args` are exec'd directly.

### `send`
In: `session_id`, `input` (string), `newline` (bool, default true: appends `\n`; Windows ConPTY uses `\r`), `wait`: `{ idle_ms (300), timeout_ms (30000, max 600000), prompt_regex?, until_regex? }`, `strip_ansi` (default true), `max_bytes`, `no_wait` (bool: write and return immediately).
Out: `output`, `done_reason` (`prompt|idle|until|timeout|exited`), `exited` (bool), `exit_code?`, `truncated`, `dropped_bytes`, `elapsed_ms`.
Writes input, flushes, waits per section 4, returns new output only. The echoed input line is stripped on a best-effort basis.

### `read`
In: `session_id`, `wait` (same object as `send`, default `idle_ms:300, timeout_ms:0` meaning return pending now), `strip_ansi`, `max_bytes`. Out: same as `send`.
Use it after a `timeout`, or for background output.

### `signal`
In: `session_id`, `signal` (`interrupt|terminate|kill|eof|suspend`; default `interrupt`). Out: `ok`, plus the same output fields after a short idle wait.
`interrupt` writes `0x03` to the PTY (so the foreground job gets SIGINT; on Windows ConPTY uses Ctrl-C input, on pipes `CTRL_BREAK_EVENT`). `eof` writes `0x04`. `suspend` is unix-only.

### `resize`
In: `session_id`, `rows`, `cols`. Out: `ok`. Returns `unsupported` for pipe mode.

### `list`
In: none. Out: `sessions[]` of `{session_id, command, args, pid, state, exit_code?, age_s, idle_s, unread_bytes, tty_mode}`.

### `kill`
In: `session_id`, `grace_ms` (default 2000). Out: `exit_code`, `final_output`, `forced` (bool).
Sends terminate, waits `grace_ms`, then kills the process tree. It removes the session. Killing an unknown ID returns `not_found`.

## 4. Knowing when output is "done" (the key problem)
An interactive process never signals "done", so `send` ends on the first condition met:
1. **exited**: the process ended. Return the remaining output and the exit code.
2. **until**: `until_regex` (per call) matches new output.
3. **prompt**: `prompt_regex` (per call, else the session's) matches the tail of the output (the last line, or a partial line, since prompts lack `\n`) after ANSI stripping. Presets:
   - bash/zsh/sh: launch exports a unique `PS1` of the form `\x1e<sid>\x1e$ ` (and `PROMPT_COMMAND` capturing `$?`) so the match cannot collide with program output. The server strips the marker and reports `last_exit_status` in the response.
   - python: `(?m)^(>>> |\.\.\. )$`. gdb: `\(gdb\) $`. psql: `\S+[=#>(\-]+ $`. node: `^> $`. cmd/powershell: `^(PS )?[A-Z]:\\.*> $`.
4. **idle**: no new bytes for `idle_ms`. This is the fallback when there is no prompt (default 300ms; the clock restarts on every byte, so a slow but steady stream keeps waiting).
5. **timeout**: `timeout_ms` reached. The process keeps running and `done_reason:"timeout"` tells the model to call `read` or `signal interrupt`.

Rule of thumb: with a prompt regex, `idle` is only a safety net. Idle must not fire before the first byte after a send until `min(idle_ms, 50ms)` has passed, and the wait starts after the write is flushed, so stale output is never mistaken for the response.

Output handling: normalize `\r\n` to `\n`; with `strip_ansi` remove CSI/OSC sequences (the raw bytes are never altered in the buffer); invalid UTF-8 becomes U+FFFD.

## 5. Output truncation
- Per response cap: `max_bytes` (default 64 KiB, hard max 1 MiB). When exceeded, keep the **head 25% + tail 75%** with a `[... N bytes omitted ...]` marker, set `truncated:true`. The omitted middle is not re-readable. Tell the model to use `| head`/`| tail` or a pager-free command.
- The ring buffer overflow (4 MiB unread) drops the oldest bytes and reports `dropped_bytes`.
- Programs that page (git, less, man) are mitigated by setting `PAGER=cat`, `TERM=dumb` unless `env` overrides.

## 6. Security
This server is **arbitrary code execution as the invoking user**. It adds no sandbox.
- No network listener, no HTTP transport, no file-based IPC. The only channel is the parent's stdio.
- Optional policy: `--allow-command <glob>` (repeatable; when set, `launch` must match the resolved absolute path). The default is allow-all, documented loudly in the README.
- `launch` takes `command` + `args`, never a shell string, so there is no injection in our layer (the user can still launch `/bin/bash`).
- Environment is inherited by default; `--clean-env` passes only `PATH,HOME,USER,LANG,TERM`.
- Logs on stderr never include input or output content unless `--log-io` is set. Sessions are not persisted to disk and are not shared across server instances.
- Human approval of tool calls is the MCP client's job. The README tells users to enable it.
- Resource bounds: session count, buffer size, max wait, and TTL reaping prevent runaway growth.

## 7. Error handling
Tool failures set `isError:true` with `{code, message}`; protocol errors are reserved for malformed requests.
Codes: `invalid_argument`, `not_found`, `limit_reached`, `spawn_failed`, `not_allowed`, `busy`, `session_exited` (write to a dead process; includes exit code and any remaining output), `unsupported`, `internal`.
A crashed process is never an error for `read`/`send`: it returns `exited:true`. Panics in a session goroutine are recovered and logged; they must not take down other sessions.

## 8. Testing plan
- **Unit**: ring buffer (wrap, drop count, cursor), ANSI stripper, head/tail truncation, wait state machine against a fake byte source with a fake clock (each `done_reason`, idle restarts, prompt split across reads), echo stripping, policy globbing, ID generation.
- **Integration** (build the binary, drive it with the SDK's client over `CommandTransport`):
  - bash: `export ONE=1` then `echo $ONE` returns `1`; `cd` persists; exit status reported.
  - two concurrent bash sessions have isolated state; parallel `send` calls on different sessions; `busy` on the same session.
  - `sleep 5` with `timeout_ms:500` returns `timeout`, then `read` collects the rest; `signal interrupt` stops `sleep 100`.
  - python REPL multi-line block; `prompt_regex` override; `until_regex`; large output (`seq 1 200000`) is truncated with the marker.
  - `kill` (graceful and forced), `exit` inside a session, TTL reaping (short TTL via flag), `--max-sessions`, server stdin close leaves no orphan processes (check pids).
  - Windows: `cmd.exe` and `powershell` over ConPTY, plus `tty:false` pipe mode; skip unix-only cases with build tags.
- CI runs `go test -race ./...`, `go vet`, `golangci-lint`, on ubuntu, macos (arm64) and windows. Tests have 60s timeouts to avoid hanging CI.

## 9. Release and maintenance
- SemVer, tags `vX.Y.Z`; `v0.x` until the tool schemas are stable. A schema-breaking change means a major bump (after 1.0). `--version` is injected via ldflags.
- `.github/workflows/ci.yml`: on push and PR, the test matrix above plus `goreleaser check`.
- `.github/workflows/release.yml`: on tag `v*`, run goreleaser: linux/darwin/windows x amd64/arm64, `CGO_ENABLED=0`, `-trimpath`, tar.gz (zip on Windows), `checksums.txt`, changelog from conventional commits.
- Install: `go install github.com/Muthu-Palaniyappan-OL/exec-wrap-mcp@latest`; release archives (a Homebrew tap is a possible follow-up). The README includes client config snippets (`{"command":"exec-wrap-mcp"}`).
- `.github/dependabot.yml`: ecosystems `gomod` and `github-actions`, weekly, grouped minor+patch.
- `.github/workflows/dependabot-automerge.yml`: on PRs by `dependabot[bot]`, use `dependabot/fetch-metadata`; for semver patch/minor run `gh pr merge --auto --squash`. Major updates get a label and need manual review. Auto-merge only completes after the required CI checks pass (branch protection requires the 3-OS matrix; repo setting "Allow auto-merge" enabled; workflow permissions `contents: write, pull-requests: write`).

## 10. Out of scope (v0.1)
Remote/HTTP transport, authentication, session persistence across server restarts, screen-scraping of full-screen TUIs (vim, htop), file transfer.
