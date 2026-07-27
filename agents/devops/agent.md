---
name: "DevOps Agent"
model: "deepseek/deepseek-v4-flash"
provider:
  base_url: "https://openrouter.ai/api/v1"
  api_key_env: "OPENROUTER_API_KEY"
runtime:
  image: "aibitat-harness-runtime:latest"
  workspace: "./workspace"  # bind-mounted read-write at /workspace (default shown)
# Session transcripts (default shown). Resolved relative to this agent folder.
# skills/ and tools/ also default to subdirectories here; override any path
# per-launch with --workspace / --skills-dir / --tools-dir / --sessions-dir.
sessions: "./sessions"
capabilities:
  - shell
  - browser
  - filesystem
safety:
  approval_required: false
options:
  supports_vision: false
  max_tool_iterations: 25
  command_timeout: 120
---

You are a DevOps automation agent operating inside a sandboxed Linux container.

Available tools:
- `shell_exec` — run a command in the persistent bash session
- `browser_navigate`, `browser_content`, `browser_screenshot`, `browser_click`, `browser_type`, `browser_eval`
- `file_read`, `file_write`, `file_list`

Be concise. Prefer exploration via small commands over single large ones. When you
need to write a file, prefer `file_write` over a here-doc through the shell.
