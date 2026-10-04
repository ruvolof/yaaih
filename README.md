# yaaih

`yaaih` is a stateful agent runtime with a TUI front end. It runs an agent defined in the `./agents/chatter` folder by default, connects to an OpenAI-compatible LLM endpoint, and executes sandboxed shell/browser/filesystem tools inside a Docker runtime container.

Each agent lives in its own folder under `agents/` — e.g. `agents/devops/` — containing `agent.md` (the definition), plus `skills/`, `tools/`, and a runtime `workspace/`. Point `--agent` at the folder; the harness reads `agent.md` inside it.

## 1) Build on Linux

This repository already keeps Go state local to the project by using a project-local `.go/` directory.

### Recommended commands

```bash
./build.sh build
```

or, if you want to prepare vendor files explicitly:

```bash
./build.sh vendor
./build.sh build
```

### What `build.sh` does

- sets `GOPATH` to `./.go`
- sets `GOMODCACHE` to `./.go/pkg/mod`
- sets `GOCACHE` to `./.go/cache`
- sets `GOBIN` to `./.go/bin`
- runs `go mod tidy` / `go mod vendor`
- builds the binary with `-mod=vendor`

### Alternate manual Linux build

```bash
export GOPATH="$PWD/.go"
export GOMODCACHE="$PWD/.go/pkg/mod"
export GOCACHE="$PWD/.go/cache"
export GOBIN="$PWD/.go/bin"
mkdir -p "$GOMODCACHE" "$GOCACHE" "$GOBIN"

go mod tidy
go mod vendor
go build -mod=vendor -o yaaih .
```

### Tests and coverage

```bash
./build.sh test
```

Runs `go vet` and then the suite under the race detector. Several packages are
exercised concurrently on purpose (the turn loop, the executor's bash session,
the governor), so a run without `-race` proves less than it appears to.

```bash
./build.sh cover
```

Same run with coverage: prints a per-package table, the overall total, and the
least-covered functions, then writes a line-by-line HTML report to
`.go/cover/coverage.html`. Set `PKG` to narrow either target to one package:

```bash
PKG=./internal/executor/ ./build.sh cover
```

Some tests need something the host may not have and skip themselves when it is
missing — the browser tab tests want a `chromium` binary and the Playwright
driver, and nothing here talks to a Docker daemon. A skip is reported as `ok`,
so read the `-v` output if you want to know which ones actually ran.

### Clean

```bash
./build.sh clean
```

## 2) OpenRouter provider support

This project uses a single OpenAI-compatible provider implementation in `internal/llm/openai_compat.go`.

### How it works

- `internal/config/config.go` loads `provider.base_url` and `provider.api_key_env` from the selected agent's `agent.md` (default folder: `./agents/chatter`).
- `Config.APIKey()` reads the API key from the environment variable named in `provider.api_key_env`.
- `main.go` creates the provider with:
  - `llm.NewOpenAICompat(cfg.Provider.BaseURL, cfg.APIKey(), cfg.Model)`
- `openai_compat.go` constructs an `openai-go` client with `option.WithBaseURL(baseURL)` and `option.WithAPIKey(apiKey)`.
- The client uses `Chat.Completions.NewStreaming(...)` so the harness can stream responses and tool calls.

### Using OpenRouter

Use a compatible base URL and model name. For example:

```yaml
provider:
  base_url: "https://openrouter.ai/v1"
  api_key_env: "OPENROUTER_API_KEY"
model: "gpt-4o-mini"
```

Then provide your OpenRouter API key, either in a `.env` file at the project root (git-ignored):

```
OPENROUTER_API_KEY=your-openrouter-api-key
```

or by exporting it before running:

```bash
export OPENROUTER_API_KEY="your-openrouter-api-key"
./build.sh run
```

Variables already present in the environment are never overwritten, so an exported value always beats the file. A missing `.env` is not an error.

### Notes

- If `provider.api_key_env` is set but the env var is empty, `config.Validate()` prints a warning but still allows startup.
- If you use a local provider that does not require auth, leave `provider.api_key_env` blank.
- The model string should match the provider's supported chat/completion model names.

## 3) Project usage

### Run the harness

```bash
./build.sh run
```

This builds the binary and runs it with the `./agents/chatter` agent folder as the default.

### Run an alternate agent

```bash
./yaaih --agent ./agents/devops
```

`--agent` takes the agent folder (the harness reads `agent.md` inside it). You can override its directories per launch with `--workspace`, `--skills-dir`, `--tools-dir`, and `--sessions-dir`.

### Switch agents at runtime

Point `--agents` at a folder of agent folders to start in multi-agent mode:

```bash
./yaaih --agents ./agents
```

This starts with **no agent selected** and shows the available agents. From the TUI:

- `/agents` — list available agents (every subfolder containing an `agent.md`).
- `/new <agent>` — start a session with that agent (e.g. `/new devops`). The harness tears down the current agent's sandbox and brings up the selected one.
- `/new` — start a fresh session with the *current* agent (errors if none is selected yet).

Switching agents starts a clean session; sessions are per-agent, so `/resume` only reaches the current agent's history. (`--agent <folder>` also enables `/agents`/`/new <other>` for that folder's siblings — it just selects one at startup instead of starting agent-less.)

### Typing in the input box

- **Enter** sends; **Alt+Enter** or **Ctrl+J** starts a new line. The box grows with what you type, up to 8 rows, then scrolls. Pasted text keeps its line breaks.
- **↑ / ↓** walk back and forth through what you have sent, slash commands included, when the box is empty or still holds an entry you recalled unchanged. Once you edit it, the arrows move the cursor again. The history lasts the whole run, across agent switches.
- **Tab** completes slash commands, and the argument of `/resume` (every agent's sessions), `/new` (agents), `/skill` (skills not loaded yet), and `/sessions` (`all`). When there is more than one match, Tab fills in as much as the matches share, then lists them.

### Reading replies

The agent's replies are drawn as Markdown — headings, lists, code blocks,
tables — once each message is complete; while one is still streaming in it
shows as plain text.

### Reviewing tool calls

Each tool call shows collapsed in the conversation: the call on one line, a
preview of its reply below. Expanded, it shows every argument and the whole
reply — the reply exactly as the model received it, so after the harness
shortens a long output, the expanded call shows the shortened text the model
worked from.

- **Click** a call to expand or collapse it.
- **Ctrl+O** expands or collapses the newest call, or the selected one.
- **Alt+↑ / Alt+↓** select an earlier or later call, scrolling it into view;
  **Enter** on an empty input box toggles the selection. Alt+↓ past the newest
  call clears it, and so does sending a message.

**Ctrl+T** copies the whole conversation with every call expanded, and each
reply as the Markdown the model wrote rather than as it is drawn.

### Docker runtime

The harness uses Docker to run tool execution in a sandbox container.

- The runtime image is declared in the agent file under `runtime.image`.
- The runtime image must be built ahead of time from `Dockerfile` via the
  build script's `image` target; the harness no longer auto-builds it on
  first run and fails fast if the image is missing locally.
- The workspace bind mount is configured by `runtime.workspace`.

Build the image on Linux/macOS:

```bash
./build.sh image
```

Or on Windows:

```powershell
.\build.ps1 image
```

The `run` target of either build script will also build the image
automatically the first time it is invoked.

## 4) Agent configuration

Agents are defined in Markdown files under `agents/` as Markdown with YAML frontmatter.

### Required fields

```yaml
name: "My Agent"
model: "<model-name>"
provider:
  base_url: "<openai-compatible-base-url>"
  api_key_env: "<ENV_VAR_NAME>"
runtime:
  image: "yaaih-runtime:latest"
  workspace: "./workspace"
```

### Optional fields

- `provider.max_session_cost_usd`: spending limit for one session, in USD (default 0, no limit)
- `provider.max_session_tokens`: token limit for one session, input and output together (default 0, no limit)
- `runtime.limits.memory`: memory the sandbox container may use, as `docker run --memory` takes it (default `2g`)
- `runtime.limits.cpus`: CPUs the sandbox container may use (default 2)
- `runtime.limits.pids`: processes and threads the sandbox container may run (default 1024)
- `runtime.run_as_host_user`: run the sandbox as your own user instead of root (default `false`)
- `capabilities`: list of enabled capabilities like `shell`, `browser`, `filesystem`
- `safety.approval_required`: `true` or `false`
- `options.max_tool_iterations`: integer
- `options.command_timeout`: seconds
- `options.max_tool_result_bytes`: bytes of tool output kept in the conversation (default 32768)
- `options.max_file_read_lines`: default page size for `file_read` (default 500)
- `options.max_context_tokens`: ceiling on one request's prompt (default 64000)
- `options.browser_timeout`: seconds a single browser operation may take (default 30)
- `options.browser_user_agent`: overrides the User-Agent the sandboxed browser sends

### Example agent

```yaml
---
name: "DevOps Agent"
model: "deepseek-chat"
provider:
  base_url: "https://api.deepseek.com/v1"
  api_key_env: "DEEPSEEK_API_KEY"
  max_session_cost_usd: 2.00
  max_session_tokens: 2000000
runtime:
  image: "yaaih-runtime:latest"
  workspace: "./workspace"
  limits:
    memory: "2g"
    cpus: 2
    pids: 1024
capabilities:
  - shell
  - browser
  - filesystem
safety:
  approval_required: true
options:
  max_tool_iterations: 25
  command_timeout: 120
  max_tool_result_bytes: 32768
  max_file_read_lines: 500
  max_context_tokens: 64000
  browser_timeout: 30
---

Your Markdown system prompt goes here.
```

### System prompt body

Anything after the YAML frontmatter is treated as the agent's system prompt.

### Skills

A skill is a Markdown file in the agent's `skills/` folder (or wherever `skills:`
/ `--skills-dir` points). It teaches the model a convention or a workflow; it
adds nothing to the tool list. Each file opens with YAML frontmatter:

```markdown
---
name: network-debug          # what the skill is loaded by; defaults to the filename
description: Diagnosing DNS, TLS and timeout problems from inside the sandbox.
always: false                # true puts it in the system prompt from the start
disabled: false              # true ignores the file
---
Always pass `-m 5` to curl so a dead host cannot hang the shell...
```

Skills are loaded **on demand**. The system prompt carries only a catalog of
each skill's name and description, and a skill's body joins the system prompt
once it is needed, for the rest of the session:

- The **model** calls the `skill_load` tool when a task matches a description.
  The tool is offered only to agents that have a skill to load, and it needs no
  approval.
- The **operator** runs `/skill <name>`. `/skills` lists every skill as
  `available`, `loaded`, or `always`.

A skill with `always: true` is in the system prompt from the start, and so is a
skill without a `description`, because the model has nothing to choose it by.
The shipped `tool-writer` agent keeps its one skill always loaded.

A loaded skill lives in the system prompt, which neither the context window nor
`/compact` ever drops, so it stays in effect however long the session runs.
There is no unloading; `/new` starts again with none loaded. The session file
records every load, so `/resume` and `--resume` bring the session's skills back,
composed from the skill files as they are now. A skill that has since been
deleted, renamed, or made `always` is dropped with a warning in the log.

Loading a skill changes the system prompt, so providers that cache the prompt
prefix re-read the conversation once at full price. Most skills are loaded near
the start of a session, while the conversation is short.

## 5) Supported configuration fields

From `internal/config/config.go`:

- `name` (string)
- `model` (string)
- `provider.base_url` (string)
- `provider.api_key_env` (string)
- `provider.max_session_cost_usd` (float)
- `provider.max_session_tokens` (int)
- `runtime.image` (string)
- `runtime.workspace` (string)
- `runtime.limits.memory` (size string, e.g. `2g`, `512m`)
- `runtime.limits.cpus` (float)
- `runtime.limits.pids` (int)
- `runtime.run_as_host_user` (bool)
- `capabilities` (array of strings)
- `safety.approval_required` (bool)
- `options.max_tool_iterations` (int)
- `options.command_timeout` (int)
- `options.max_tool_result_bytes` (int)
- `options.max_file_read_lines` (int)
- `options.max_context_tokens` (int)
- `options.browser_timeout` (int)
- `options.browser_user_agent` (string)

### Approvals

With `safety.approval_required: true`, every restricted tool call (`shell_exec`,
`file_write`, `browser_eval`, and script tools) waits for an answer in a panel
docked above the input. The conversation stays in view above it and can still be
scrolled (`pgup`/`pgdn`, `ctrl+home`/`ctrl+end`, the wheel) and its tool calls
expanded (click, `ctrl+o`, `alt+↑/↓`) while the call waits.

| Key | Effect |
| --- | --- |
| `y` | run this call |
| `a` | run this call, and stop asking about this tool for the rest of the session |
| `A` | run this call, and stop asking about any tool for the rest of the session |
| `n` | refuse this call; the model is told and may try something else |
| `q` / `esc` | refuse this call and end the turn |
| `c` (or click the panel title) | hide or show the call's arguments |
| `↑`/`↓`, `shift+↑`/`shift+↓`, `home`/`end`, wheel over the panel | scroll the arguments |

The panel takes at most half the screen; longer arguments scroll inside it.

The sidebar's *Approval* section shows what currently runs without asking.
Allowances last until the session ends: `/new` and `/resume` start asking again.

A script tool's manifest can opt out of approval with `"restricted": false`, but
only once it is in the agent's `tools/` folder. A draft the agent wrote into
`<workspace>/tools` (with `enable_workspace_tools: true`) is always restricted,
whatever its manifest says, since the agent wrote the manifest too. `/reload`
lists the tools it loaded and marks the drafts and any that skip approval.

### Spending limits

An agent left to run on its own against a paid API has nothing else to stop it
spending: a loop between two tools goes on until the iteration cap, turn after
turn. `provider.max_session_cost_usd` and `provider.max_session_tokens` cap one
session. Tokens count input and output together — the conversation is re-sent on
every request, so input is most of the bill. Zero, or leaving a field out, means
no limit.

The limits are checked before every model request, `/compact` included. Once one
is reached, the turn ends with an error naming it and nothing more is sent in
that session; tool calls the model had already asked for still run and are
answered, so the history stays sendable. A session's spend is restored when it
is resumed, so resuming does not reset the count, while `/new` starts a session
at zero under the same limits. To carry on with a session that hit its limit,
raise the limit in `agent.md` and relaunch with `--resume`.

The sidebar shows the session's total and cost against the limits. At 80% of
either, the figures are highlighted and the conversation gets a one-off warning.

The cost limit relies on the provider reporting cost (OpenRouter does). Against
one that does not, the cost stays `N/A` and the limit cannot be enforced — the
conversation says so after the first request. Use `max_session_tokens` there.

### Sandbox limits

The sandbox container is capped so that a runaway command — a fork bomb, a
memory leak, a busy loop in a tool the agent wrote — is stopped inside it rather
than starving the host. The defaults are 2 GB of memory, 2 CPUs, and 1024
processes; `runtime.limits.memory`, `runtime.limits.cpus`, and
`runtime.limits.pids` raise or lower them per agent. A limit can be raised but
not removed.

- **Memory** is a hard ceiling with no swap on top. When it is reached, the
  kernel kills the largest process in the container — normally the command at
  fault, which the shell then reports as `Killed`, or Chromium if the browser is
  the one using the most. A killed browser shows up as a lost connection on the
  next browser tool call.
- **CPUs** above what the Docker host has are lowered to what it has, with a
  note at startup. Docker Desktop's VM often has fewer CPUs than the machine.
- **PIDs** counts threads as well as processes. Chromium alone runs a few
  hundred; lower the limit much below the default and the browser starts
  failing to open pages. A fork bomb stops at the limit, but whatever it
  started keeps the sandbox full until the container is replaced — by
  relaunching or by switching agents; `/new` alone keeps the same container.
  The host is unaffected.

The container also runs with `no-new-privileges`: setuid binaries in the image
cannot raise a process's privileges.

#### Running as your own user

By default the sandbox runs as root. With native Docker Engine on Linux that
means files the agent creates in the workspace are owned by `root` on the host,
and you need `sudo` to edit or delete them. `runtime.run_as_host_user: true`
runs the container as your uid:gid instead, so they are yours.

The cost is root inside the sandbox: the agent cannot `apt-get install`, and
tools that need raw sockets (`nmap -sS`, `tcpdump`) do not work. `pip install
--user` and `npm` with a local prefix still do. Unless your uid happens to exist
in the image (1000 is the Playwright base's `pwuser`), it has no name there and
`whoami` fails; `HOME` is set to `/tmp` so the browser and the
usual dotfile-writing tools have somewhere to write. Files the agent created as
root in an earlier session stay root-owned, and the agent can no longer change
them — `sudo chown -R $(id -u):$(id -g)` the workspace once when switching.

The setting is ignored, with a note at startup, where it is not needed: on a
rootless Docker daemon container root already is you, and on Windows Docker
Desktop already hands workspace files to your user. Docker Desktop on Linux and
macOS does the same through its file sharing, but the setting still applies
there, for the root it takes away.

### Tool output size

Tool results are replayed to the provider on every later request in the session,
so an unbounded one is charged again on every turn that follows it. Before a
result is recorded the harness strips terminal noise that carries no information
— ANSI escapes, carriage-return progress repaints, runs of identical lines — and
then bounds whatever is left to `options.max_tool_result_bytes`, keeping the head
and the tail.

Nothing is lost when that bound bites. The full output is written to
`<workspace>/.harness/tool-output/`, and the truncation marker names the file so
the agent can `grep` it (with `shell_exec`) or page through it (with
`file_read`). That directory is pruned oldest-first once it passes 64 MiB.

`file_read` returns `options.max_file_read_lines` lines at a time with a header
naming the range and the file's total line count, so reading part of a large file
is the normal path rather than a workaround.

### Browser

`browser_navigate` returns once the HTML is parsed. It does not wait for the
network to go quiet: analytics beacons, websockets, and long-polling keep a
request in flight for as long as a page is open, so waiting for idle turns an
ordinary page load into a timeout that looks like a blocked request. The
trade-off is that content a page renders from script may not be there yet —
`browser_wait_for` waits for a named selector to become `visible`, `hidden`,
`attached`, or `detached`, which is also how you wait out a spinner.

Every browser operation is bounded by `options.browser_timeout` (default 30s),
including clicks and typing, which would otherwise sit on playwright's own 30s
default regardless of what you configured.

`browser_content` returns the page as Markdown, led by its title and current URL
— which is how the agent learns where a redirect actually landed. It reads
`<main>` or `<article>` when the page marks one (falling back to the whole body
if that turns out to be an empty shell), drops `nav`, `footer`, `aside`, and
anything `hidden` or `aria-hidden`, and keeps headings, lists, tables, and code
blocks. Link targets come out as absolute URLs, so a link in the output can be
handed straight back to `browser_navigate`. A single extraction stops at 200k
characters with a marker saying so; what the model actually sees is then bounded
by `options.max_tool_result_bytes` in the usual way, with the remainder spilled
to disk.

Clicking a link that opens in a new tab leaves the harness holding the old page,
which otherwise looks like a click that did nothing. `browser_tabs` lists what is
open and marks the tab the other tools act on, `browser_select_tab` moves across
by index, and `browser_close_tab` closes one — moving off it first if it is the
active tab, and opening a replacement if it is the last one, so the browser is
never left without a usable page. `browser_back` steps back through the current
tab's history.

The sandboxed Chromium presents itself as ordinary desktop Chrome: a real
desktop User-Agent derived from its own version rather than the default
`HeadlessChrome`, a 1920x1080 window, `en-US`, and none of the automation
markers a routine fingerprinting script reads. Without this most commercial bot
filters serve a challenge page instead of content, and the 800x600 headless
default gets you mobile layouts. Set `options.browser_user_agent` to override the
string. This is enough for naive fingerprinting and is not intended to go
further — there is no CAPTCHA solving here.

### Context window

The whole conversation is re-sent on every request, so a long session eventually
outgrows the model — and once it has, every turn fails, including a trivial one.
`options.max_context_tokens` (default 64000) is the ceiling on one request:
system prompt, tool definitions, and conversation together. Set it below the
model's real context window, leaving room for the reply, and lower it for a small
local model.

Above the ceiling the oldest exchanges stop being sent, an assistant message and
the tool results answering it at a time, and the model is told how many messages
it can no longer see. Two things are never dropped: the system prompt, and the
turn in flight — the question being answered and every result it has produced —
so a conversation that crosses the ceiling mid-turn keeps going rather than
breaking. When one turn is too large to fit even on its own, its oldest tool
results are shortened instead, first to their head and tail and then to nothing
but a note that the tool ran; the call itself always survives, so the model can
see what it has already done.

This bounds what is *sent*. The session file keeps everything, so a resumed
transcript still shows the whole conversation. The sidebar's `Context` figure is
what the last request cost against the ceiling, and once it passes 85% the
conversation gets a one-off warning naming `/compact`, so the ceiling is not
first met as a failed turn.

A budget has a floor. The system prompt and the tool definitions go out on every
request and cannot be windowed away, so a ceiling close to that floor leaves
nothing for the conversation — for the shipped `devops` agent the floor is
several hundred tokens, and a ceiling of a couple of thousand will keep hitting
it. A turn that cannot be made to fit ends with an error naming both numbers
rather than paying for a request the provider would reject, and the turn after
it succeeds, because by then the failed turn is old enough to be windowed away.

Trimming is lossy: the model genuinely stops being able to see what left the
window. `/compact` replaces the earlier conversation with a summary of it, at
the cost of one extra model call. It keeps the last turn verbatim; where the
session *is* one long turn — one question and twenty tool calls, which is how a
context usually fills — it keeps the last exchange instead, so compaction is
available exactly when it is the way out. The swap is recorded in the session
file, so a resumed session stays compacted.

## 6) Notes on the runtime container

The runtime Docker image is defined in `Dockerfile` and is based on Playwright.
It includes shell utilities, Chromium tooling, and mounts the agent workspace at `/workspace`.

Networking is not restricted: the container has the same outbound access as the
host, which the browser and package installs need. Treat what the agent can
reach from the sandbox as what it can reach from your machine. See
[Sandbox limits](#sandbox-limits) for what is capped.

## 6b) Logs and crash diagnostics

The TUI owns the terminal during a run, so neither stdout nor stderr is
useful for diagnostics live. Everything goes to a per-session log file:

```
./logs/harness-<YYYYMMDD-HHMMSS>.log
```

One file per run; old files stay around until you delete them. On a fatal
exit, the harness prints `see log: ./logs/harness-…log` to stderr so you
know which file to open.

### What lands there

- **Structured beacons** for every major flow point — see the component
  table below.
- Anything written to `os.Stderr`. Stderr is redirected to the log file
  before the TUI starts; on Linux/macOS this also catches Go's runtime
  panic dump via `dup2`. On Windows the runtime caches its stderr handle
  at startup, so runtime panic dumps from goroutines we do not own may
  still print to the console — panics in our own goroutines are caught
  by an explicit recover and routed through the logger first.
- Errors from the LLM, gatekeeper, tools, and Docker.
- Standard `log.Printf` output from third-party libraries that use the
  stdlib logger.

### Format

Plain text via `log/slog`'s `TextHandler`, with the time key shortened to
`HH:MM:SS.mmm` and the level padded to fixed width. Each event is one line:

```
t=07:44:12.103 level=INFO  msg="request sent" component=llm model=deepseek-chat msgs=3 tools=6
t=07:44:12.480 level=INFO  msg="stream first chunk" component=llm latency_ms=377
t=07:44:14.220 level=INFO  msg="stream done" component=llm tokens_in=412 tokens_out=88 chunks=44 elapsed_ms=2117
t=07:44:14.221 level=INFO  msg="tool call" component=agent name=shell_exec id=call_x args_len=42
t=07:44:14.301 level=INFO  msg="result" component=executor cmd="ls -la /workspace" out_bytes=512 elapsed_ms=77 cwd=/workspace
```

Grep / `findstr` is the intended workflow:

```bash
# every LLM round-trip
grep '"component=llm"' logs/harness-*.log

# everything from one component
grep 'component=agent' logs/harness-*.log

# only errors
grep 'level=ERROR' logs/harness-*.log
```

### Verbosity

Pass `--log-level` to set the level (default `info`):

```bash
./yaaih --log-level debug
```

Recognised values: `debug`, `info`, `warn`, `error`. Debug adds finer-grained
events (each `executor send`, every agent iteration counter, cwd changes).

### Components / beacons

| component   | what you can expect at INFO                                                              |
| ----------- | ---------------------------------------------------------------------------------------- |
| `logging`   | session started / ended, log file path, level, pid                                       |
| `main`      | agent loaded (model, provider, capabilities), runtime required, TUI start/exit, signals  |
| `config`    | api-key env warning when set but empty                                                   |
| `docker`    | daemon reachable (api version), image verified, container started + cdp port, container stopped, orphan cleanup |
| `executor`  | bash session started (container id, cwd, timeout), each command result (bytes, elapsed)  |
| `browser`   | CDP connected, navigate, screenshot saved (path)                                         |
| `llm`       | provider configured, request sent (model, msgs, tools), first chunk latency, stream done (tokens, chunks, elapsed), errors |
| `agent`     | turn start (user msg len, history depth), tool calls dispatched, tool ok / failed, denied by operator, max iterations, skill loaded (name, by model or operator) |
| `skills`    | skill disabled, skill name already used, skill without a description loaded from the start |
| `gatekeeper`| approval requested, approved, denied                                                     |
| `governor`  | per-turn usage delta + running totals                                                    |
| `panic`     | goroutine panics — value + full stack                                                    |

### What is deliberately not logged

- **LLM request and response bodies.** Only metadata (token counts, message
  counts, latencies). If you ever need to log the actual prompts, change
  `internal/llm/openai_compat.go` — but be aware that log files contain
  whatever you put in them.
- **API keys.** The provider only logs whether a key is set, never its value.
- **Full subprocess stdout.** Truncated for the log; the model still sees
  the full output.

## 7) Recommended workflow

1. Build locally:
   ```bash
   ./build.sh build
   ```
2. Put the provider API key in `.env` at the project root (or export it):
   ```bash
   echo 'OPENROUTER_API_KEY=...' > .env
   ```
3. Run the harness:
   ```bash
   ./build.sh run
   ```
4. Edit `./agents/chatter/agent.md` or `./agents/devops/agent.md` to change prompt, provider, runtime, or safety settings.
