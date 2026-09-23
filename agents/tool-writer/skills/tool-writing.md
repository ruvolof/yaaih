---
name: tool-writing
description: How to author new script tools for this harness.
---
You can extend yourself by writing new **script tools**. A tool is two files
that share a base name: an executable script and a JSON manifest. You author
them in the workspace; the operator reviews them and promotes the good ones to
the permanent tool folder.

## Where to write them

Use `file_write` to create both files under `tools/` in the workspace (paths
are relative to the workspace root):

- `tools/<name>.<ext>` — the executable (e.g. `tools/check_url.sh`)
- `tools/<name>.json` — the manifest

These land at `/workspace/tools/` inside the sandbox, which is exactly where
the harness runs them. When the harness is configured with
`enable_workspace_tools: true`, the operator can run `/reload` to load them
right away — no copy step needed for testing.

## Manifest format

```json
{
  "name": "check_url",
  "description": "Fetch a URL and report its HTTP status code and total time.",
  "executable": "check_url.sh",
  "parameters": {
    "type": "object",
    "properties": {
      "url": { "type": "string", "description": "The URL to fetch." },
      "timeout_seconds": { "type": "integer", "default": 5 }
    },
    "required": ["url"]
  },
  "restricted": true,
  "timeout_seconds": 30
}
```

Field rules:
- `name` — required, must match `^[a-z][a-z0-9_-]*$`, unique among tools.
- `description` — required; tell the model clearly *when* and *how* to use it.
- `executable` — required; the script's filename, relative to the manifest.
- `parameters` — required; a JSON Schema object describing the arguments.
- `restricted` — optional, defaults to `true`. Leave it `true` so the operator
  approves each call. A draft in the workspace is always restricted; an
  explicit `false` only takes effect once the operator promotes the tool.
- `timeout_seconds` — optional, defaults to `30`.

## Calling convention (important)

When the model invokes the tool, the harness writes the arguments as a JSON
object to a temporary file and passes that file's **path as the first argument
(`$1`)**. Your script must read and parse that file. Do **not** read from
stdin, and do not expect arguments as flags.

- Print your result to **stdout** — that text becomes the tool result.
- Exit `0` on success.
- On failure, print a helpful message (the **last line** of stdout is shown to
  the model as the error) and exit non-zero.

## Environment

Scripts run inside the Linux sandbox container. Drafts in `tools/` run as
`/workspace/tools/<exe>`. Start with a shebang (`#!/usr/bin/env bash` or
`#!/usr/bin/env python3`). For drafts in the workspace the harness sets the
executable bit for you. Before relying on a CLI (`jq`, `curl`, `python3`, …),
confirm it exists with `shell_exec` (e.g. `command -v jq`); if it is missing,
choose another approach.

## Example

`tools/check_url.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

args_file="$1"
url=$(jq -r '.url' "$args_file")
timeout=$(jq -r '.timeout_seconds // 5' "$args_file")

if [ -z "$url" ] || [ "$url" = "null" ]; then
  echo "missing required argument: url"   # last line -> shown to the model
  exit 1
fi

curl -s -o /dev/null -w 'status=%{http_code} time=%{time_total}s\n' \
  --max-time "$timeout" "$url"
```

## Workflow

1. Decide the tool's single purpose and its inputs.
2. Verify required interpreters/CLIs with `shell_exec`.
3. `file_write` the script and the manifest into `tools/`.
4. Validate inputs in the script and fail fast with a clear message.
5. Tell the operator the tool is ready; they can `/reload` to load it and will
   promote vetted tools into the permanent tool folder.
