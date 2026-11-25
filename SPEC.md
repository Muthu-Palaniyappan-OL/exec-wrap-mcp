# exec-wrap-mcp: Spec (v0.1.1)

An MCP server (stdio only) that wraps long-lived interactive programs (bash, gdb, lldb, python, psql, ...) as persistent **sessions**. State survives across tool calls (`export ONE=1` then `echo $ONE` prints `1`). Many sessions run at once.

## 1. Stack and constraints
- Go 1.25+, one static binary (`CGO_ENABLED=0`). Stdio transport only; stdout carries MCP frames, logs go to stderr.
- MCP: `github.com/modelcontextprotocol/go-sdk`. Unix PTY and Windows ConPTY via `github.com/aymanbagabas/go-pty`; `pipe` mode (plain pipes) on any OS and as ConPTY fallback.
- Cleanup: each child leads its own process group (setsid/setpgid); signals reach the whole job. Windows uses `taskkill /T`. All sessions die when the server exits.

## 2. Token budget (design rule)
Every byte of schema and result is paid for in LLM tokens, so:
- **4 tools**: `launch`, `send`, `kill`, `list`. Hand-written minimal input schemas (about 2.2 KB for all four, down from 8.4 KB for 7 tools in v0.1.0). A test enforces a 3000-byte budget.
- **Text-only results by default**: the program's output, plus a one-line `[note]` only when something is unusual. No JSON duplicate, no output schema. `--structured` adds `structuredContent` for programmatic clients.
- Empty successful reply is `ok`.
- Fewer calls: `launch` takes `input`, so "start Python and run X" is one call. Sessions expire on their own, so `kill` is optional.

## 3. Tools
Arguments are flat; zero/empty means "default".

### `launch`
`command` (required), `args`, `cwd`, `env`, `input` (first input, sent once ready), `wait_ms` (readiness + first reply, default 30000), `preset`, `prompt_regex`, `ready_regex` (startup-finished pattern for prompt-less programs), `pipe` (no terminal), `rows`, `cols` (default 40x200).
Reply: first line is the session id, then the output. `[starting: ...]` if the program is not ready within `wait_ms`.
`command` + `args` are exec'd directly; no shell is involved.

### `send`
`session_id` (required), `input`, `key` (`ctrl-c|ctrl-d|ctrl-z|term|kill`; exclusive with `input`), `raw` (no newline appended), `wait_ms` (default 30000, max 600000), `idle_ms`, `until` (regex), `no_wait`.
- With `input`: writes it, flushes, waits per section 5, returns the new output.
- With neither `input` nor `key`: reads more output (use after a timeout). `no_wait` returns what is pending now.
- With `key`: delivers the key or signal and returns the resulting output.

### `kill`
`session_id`. Terminates gracefully (SIGHUP+SIGTERM, 2 s), then force-kills; removes the session. Reply is `ok` or any final output.

### `list`
No arguments. One line per session: `id command state` where state is `starting`, `running` or `exited N`.

### Notes appended to replies
`[exit N]` (last shell command failed), `[exited N]`, `[killed by SIGKILL]`, `[timeout: still running; ...]`, `[idle: no prompt seen ...]`, `[starting: ...]`, `[truncated]`, `[dropped NB]`.

## 4. Session model and lifecycle
- ID `s_` + 8 hex chars. States: `starting` (until the first prompt/ready pattern), `running`, `exited` (keeps exit code and unread output).
- **Startup readiness.** A session with a known prompt or `ready_regex` starts as `starting`. The reader goroutine detects the pattern as bytes arrive. `launch` waits up to `wait_ms`. If the program is still loading, `launch` returns `[starting]`, and the process keeps running. A later `send` waits for readiness first (within its own `wait_ms`), then writes; if it is not ready in time, nothing is written (`[starting]`). After one timed-out wait, a program that has produced output and then stays silent for 3 s counts as ready, so a wrong prompt pattern cannot wedge a session. Shell setup noise and banners printed before readiness are discarded (banners of non-shell programs are returned with the first reply).
- **Limits.** `--max-sessions 16`, `--idle-ttl 30m` (no tool call; sessions in the middle of a call are never reaped), `--max-buffer 4 MiB` per session (oldest dropped, counted), `--max-output 65536` per reply.
- **Dead sessions.** A process that exits (or is killed by a signal) leaves an `exited` session: `send` returns `session_exited: ... (code N)` with leftover output; `send` with no input still returns the leftover output plus `[exited N]`. Signal deaths report the shell convention `128+N` and the signal name. Exited sessions are evicted (oldest first) only when a new `launch` needs the slot.
- **Tombstones.** The last 64 removed ids are remembered. `send`/`kill` on them returns `session_closed: session s_x was ended by kill | was closed after 30m0s without use | had exited (code N) and was cleaned up`, each saying state is lost and to launch again. Only ids never seen return `not_found`.
- A second call into a busy session returns `busy`. Different sessions are fully concurrent.

## 5. Knowing when output is "done"
`send` returns on the first of: **exited**, **until** (`until` regex matched), **prompt** (prompt regex matches the cleaned tail, after at least one new byte and 25 ms of quiet so multi-command input is not cut short), **idle** (no new bytes for `idle_ms`; 300 ms default, 5 s when a prompt is known), **timeout** (`wait_ms`; the process keeps running).
Presets: bash/sh/zsh (private marker prompt: an ASCII record separator around the last exit status, installed at launch; bash and zsh run without line editing and with echo off; the marker is stripped and reported as `[exit N]`), python, gdb, lldb, psql, node, cmd, powershell, none.
Output is normalised: `\r\n` to `\n`, ANSI/OSC sequences removed, `\r` and `\b` applied like a dumb terminal, invalid UTF-8 replaced. Replies over `--max-output` keep head 25% and tail 75% with a marker. Children get `TERM=dumb`, `PAGER=cat` unless `env` overrides.

## 6. Security
Arbitrary code execution as the invoking user; no sandbox. Stdio only, no listener. `launch` takes `command` + `args`, never a shell string. `--allow-command <glob>` (repeatable) restricts executables. `--clean-env` passes only `PATH,HOME,USER,LANG,TERM`. I/O is logged only with `--log-io`.

## 7. Errors
Tool errors set `isError` with text `code: message`. Codes: `invalid_argument`, `not_found`, `session_closed`, `limit_reached`, `spawn_failed`, `not_allowed`, `busy`, `session_exited`, `unsupported`, `internal`.

## 8. Testing
Unit tests: output cleaning, truncation, presets/prompt regexes, echo stripping, env merge. End-to-end tests build the binary and drive it with the SDK client (Linux every change; macOS and Windows on demand): state persistence, isolation, multi-line input, timeout then read, Ctrl-C, truncation, exit/kill, reaping (and not reaping a busy session), allow-list and limits, Python and lldb sessions, slow-starting Python and bash, signal deaths, eviction, text-mode output and the schema size budget.

## 9. Release and maintenance
SemVer, tags `vX.Y.Z`, GoReleaser (linux/darwin/windows x amd64/arm64) on tag. CI: one Linux job (`ci-ok`). Dependabot opens PRs for Go modules and Actions and auto-merges them once `ci-ok` passes; a 6-hourly sweep re-enables auto-merge on any that missed it.

## 10. Out of scope
Remote/HTTP transport, authentication, persistence across server restarts, full-screen TUIs (vim, htop), file transfer.
