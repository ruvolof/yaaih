# yaaih

`yaaih` is a stateful agent runtime with a terminal UI. It talks to any
OpenAI-compatible LLM endpoint and runs every tool that touches the world —
shell, browser, filesystem, your own scripts — inside a per-session Docker
sandbox.

An agent is a folder under `agents/` holding an `agent.md` (settings plus system
prompt), and optionally `skills/`, `tools/`, and a `workspace/` the sandbox works
in.

## Quickstart

You need Go (version in `.go-version`) and Docker. Go state stays inside the
repository under `.go/`, so nothing is installed system-wide.

1. Put your provider key in `.env` at the repository root (git-ignored). The
   shipped agents use OpenRouter:

   ```
   OPENROUTER_API_KEY=sk-or-...
   ```

   An exported variable wins over the file.

2. Build and run:

   ```bash
   ./build.sh run
   ```

   This builds the binary and, the first time, the sandbox image
   `yaaih-runtime:latest`, then starts the `chatter` agent. To build them
   separately: `./build.sh build` and `./build.sh image`. The harness never builds
   or pulls the image itself; it stops with an error if the image is missing.

3. Try another agent:

   ```bash
   ./yaaih --agent ./agents/devops
   ```

On Windows, `.\build.ps1` takes the same targets (`build`, `image`, `run`,
`clean`, …).

## Shipped agents

| Agent | Capabilities | What it is for |
| --- | --- | --- |
| `chatter` | none | Plain conversation. No tools, so no sandbox and no Docker needed. |
| `devops` | `shell`, `browser`, `filesystem` | General work inside the sandbox. Approvals off. |
| `tool-writer` | the above plus `scripts` | Writes new script tools for you to review and promote. Approvals on. |

Copy one of these folders to start your own.

## Writing an agent

```
agents/my-agent/
├── agent.md      # required
├── skills/       # Markdown skills (optional)
├── tools/        # script tools (optional)
├── workspace/    # mounted read-write at /workspace in the sandbox
└── sessions/     # saved transcripts, written by the harness
```

`agent.md` is YAML frontmatter followed by the system prompt:

```markdown
---
name: "My Agent"
model: "deepseek/deepseek-v4-flash"
provider:
  base_url: "https://openrouter.ai/api/v1"
  api_key_env: "OPENROUTER_API_KEY"
  max_session_cost_usd: 2.00
capabilities:
  - shell
  - filesystem
safety:
  approval_required: true
---

You are an agent that ... (everything below the frontmatter is the system prompt)
```

Only `name`, `model`, and `provider.base_url` are required. Any
OpenAI-compatible endpoint works: point `base_url` at it and use its model
names. For a local server that needs no key, leave out `api_key_env`.

### Capabilities and tools

Capabilities decide which built-in tools the model gets. Declaring any of them
starts the Docker sandbox; an agent with none runs without Docker.

| Capability | Tools |
| --- | --- |
| `shell` | `shell_exec`: commands in a persistent bash session |
| `filesystem` | `file_read`, `file_write`, `file_list` (paths inside the workspace) |
| `browser` | `browser_navigate`, `browser_content` (page as Markdown), `browser_click`, `browser_type`, `browser_wait_for`, `browser_screenshot`, `browser_eval`, `browser_back`, `browser_tabs`, `browser_select_tab`, `browser_close_tab` |
| `scripts` | the agent's script tools (see below) |

`skill_load` is added automatically when the agent has on-demand skills.

### Configuration reference

Paths are relative to the agent folder. Limits of `0` mean "no limit".

| Field | Default | Meaning |
| --- | --- | --- |
| `name`, `model` | required | Display name; model id as the provider names it |
| `provider.base_url` | required | OpenAI-compatible endpoint |
| `provider.api_key_env` | — | Environment variable holding the API key |
| `provider.max_session_cost_usd` | `0` | Spending cap per session (needs a provider that reports cost, such as OpenRouter) |
| `provider.max_session_tokens` | `0` | Token cap per session, input and output together |
| `runtime.image` | `yaaih-runtime:latest` | Sandbox image |
| `runtime.workspace` | `./workspace` | Host folder mounted at `/workspace` |
| `runtime.limits.memory` / `cpus` / `pids` | `2g` / `2` / `1024` | Sandbox resource caps |
| `runtime.run_as_host_user` | `false` | Run the sandbox as your uid instead of root (see [Sandbox](#sandbox)) |
| `skills` / `tools` / `sessions` | `./skills` / `./tools` / `./sessions` | Folder locations |
| `enable_workspace_tools` | `false` | Also load draft tools from `<workspace>/tools` |
| `capabilities` | none | See above |
| `safety.approval_required` | `false` | Ask before restricted tool calls |
| `options.max_tool_iterations` | `25` | Tool calls the model may make in one turn |
| `options.command_timeout` | `120` | Seconds a shell command may run |
| `options.max_context_tokens` | `64000` | Ceiling on one request's prompt; set it below the model's context window |
| `options.max_tool_result_bytes` | `32768` | Tool output kept in the conversation; the rest is saved under `<workspace>/.harness/tool-output/` |
| `options.max_file_read_lines` | `500` | Lines `file_read` returns per call |
| `options.browser_timeout` | `30` | Seconds a browser operation may take |
| `options.browser_user_agent` | Chrome's own | Override the browser's User-Agent |

`--workspace`, `--skills-dir`, `--tools-dir`, and `--sessions-dir` override the
folder paths for one launch.

## Skills

A skill is a Markdown file in `skills/` that teaches the agent a convention or a
workflow. It adds no tools.

```markdown
---
name: network-debug
description: Diagnosing DNS, TLS and timeout problems from inside the sandbox.
always: false
disabled: false
---
Always pass `-m 5` to curl so a dead host cannot hang the shell...
```

The system prompt lists each skill's name and description. The body is added to
the system prompt only when the model calls `skill_load` or you run
`/skill <name>`, and it stays until the session ends. Skills with
`always: true`, or without a `description`, are included from the start.

## Script tools

A script tool is two files in the agent's `tools/` folder with the same base
name: an executable and a JSON manifest. The agent needs the `scripts`
capability to use them.

```json
{
  "name": "check_url",
  "description": "Fetch a URL and report its HTTP status code and total time.",
  "executable": "check_url.sh",
  "parameters": {
    "type": "object",
    "properties": { "url": { "type": "string" } },
    "required": ["url"]
  },
  "restricted": true,
  "timeout_seconds": 30
}
```

- The script runs inside the sandbox. Its first argument (`$1`) is the path of a
  JSON file holding the call's arguments.
- Whatever it prints to stdout is the tool result. On failure, exit non-zero;
  the last line of output is shown to the model as the error.
- `restricted` (default `true`) means the call needs approval when approvals
  are on. `timeout_seconds` defaults to 30.
- `tools/` is mounted read-only at `/tools`, so the agent cannot change its own
  tools. Make the script executable (`chmod +x`) before you run `/reload`.

The easiest way to get new tools is the `tool-writer` agent. It writes drafts
into its `<workspace>/tools`. With `enable_workspace_tools: true`, `/reload` loads
them for testing, and they always need approval whatever their manifest says.
Once a draft looks right, copy both files into `tools/` to promote it. The
`tool-writing` skill in `agents/tool-writer/skills/` documents the full format.

## Safety and limits

**Approvals.** With `safety.approval_required: true`, `shell_exec`, `file_write`,
`browser_eval`, and restricted script tools wait for your answer in a panel above
the input:

| Key | Effect |
| --- | --- |
| `y` | run this call |
| `a` | run it and stop asking about this tool for the session |
| `A` | run it and stop asking about any tool for the session |
| `n` | refuse; the model is told and may try something else |
| `q` / `esc` | refuse and end the turn |

**Spending.** `max_session_cost_usd` and `max_session_tokens` are checked before
every request. Once a cap is reached, the session stops sending requests. A
resumed session keeps its earlier spend. To continue, raise the limit and
relaunch with `--resume`.

<a id="sandbox"></a>
**Sandbox.** The container is capped by `runtime.limits` (2 GB memory with no
swap, 2 CPUs, 1024 processes and threads) and runs with `no-new-privileges`.
Chromium alone uses a few hundred threads, so set `pids` much lower and the
browser stops working. **The network is not restricted:** whatever the agent can
reach from the sandbox, it can reach from your machine.

By default the sandbox runs as root. On native Docker Engine on Linux, files the
agent creates in the workspace are then owned by root on the host.
`runtime.run_as_host_user: true` runs it as your uid:gid instead. The agent then
cannot `apt-get install` or use raw sockets. Run
`sudo chown -R $(id -u):$(id -g)` on the workspace once to take back files
created earlier. On rootless Docker and Windows the setting is ignored because
it is not needed.

**Context.** Every request re-sends the whole conversation. Above
`max_context_tokens`, the oldest exchanges stop being sent, and the model is told
they were dropped. The session file still keeps everything. `/compact` replaces
the earlier conversation with a summary, at the cost of one extra model call.
The sidebar shows how full the context is, and the conversation warns you at
85%.

## Using the TUI

### Flags

| Flag | Effect |
| --- | --- |
| `--agent <folder>` | Start with this agent; its sibling folders can be reached with `/new` |
| `--agents <folder>` | Start with no agent selected and pick one with `/new <name>` |
| `--resume <id>` | Reopen a saved session |
| `--new-session=false` | Do not save this run |
| `--log-level` | `debug`, `info` (default), `warn`, `error` |

### Commands

| Command | Effect |
| --- | --- |
| `/help` | List commands |
| `/agents` | List available agents |
| `/new [agent]` | Fresh session with the current agent, or switch to another |
| `/sessions [all]` | List saved sessions for this agent, or for all agents |
| `/resume <id>` | Resume a saved session |
| `/save <name>` | Rename the current session |
| `/skills`, `/skill <name>` | List skills; load one |
| `/reload` | Reload script tools |
| `/compact [focus]` | Summarise the earlier conversation |
| `/exit` | Quit (also Ctrl+C) |

### Keys

| Key | Effect |
| --- | --- |
| Enter / Alt+Enter or Ctrl+J | Send / new line |
| ↑ / ↓ on an empty input | Walk through what you have sent |
| Tab | Complete commands, agent names, session ids, skill names |
| Click, Ctrl+O | Expand or collapse a tool call |
| Alt+↑ / Alt+↓ | Select an earlier or later tool call |
| PgUp / PgDn, mouse wheel | Scroll the conversation |
| Drag, then Ctrl+Y | Copy the selection |
| Ctrl+Y with nothing selected | Copy the last reply as Markdown |
| Ctrl+T | Copy the whole conversation |

## Logs

The TUI takes over the terminal, so diagnostics go to
`./logs/harness-<YYYYMMDD-HHMMSS>.log`, one file per run. If the harness exits
on a fatal error, it prints `see log: …` with the file's path. Use
`--log-level debug` for more detail. Prompts, replies, and API keys are never
logged.
