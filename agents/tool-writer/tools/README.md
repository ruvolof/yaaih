# Tools folder

This is the **persistent tool folder** for this agent (its `tools:` config, or
the default `tools/` next to `agent.md`). Each tool is a pair of files sharing a
base name:

- `<name>.json` — the manifest (see the `tool-writing` skill for the format)
- `<name>.<ext>` — the executable script

This folder is bind-mounted **read-only** into the sandbox at `/tools`, so the
agent's own shell cannot rewrite a promoted tool. Tools here run from
`/tools/<exe>`.

## Promoting a drafted tool

The Tool Writer agent drafts candidate tools into its **workspace**
(`<workspace>/tools/`, which runs at `/workspace/tools/`). To promote one:

1. Review the script and its manifest.
2. Copy both files here.
3. Because the `/tools` mount is read-only, the harness cannot set the
   executable bit at run time — make sure the script is executable
   (`chmod +x <name>.<ext>`) before or after copying. (On Windows hosts the
   bind mount already exposes files as executable, and Git tracks the bit.)
4. `/reload` (or restart) to register it from `/tools`.

Only `*.json` manifests (with their referenced executables) are loaded; other
files such as this README are ignored.
