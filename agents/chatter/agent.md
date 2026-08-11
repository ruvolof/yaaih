---
name: "Chatter"
model: "deepseek/deepseek-v4-flash"
provider:
  base_url: "https://openrouter.ai/api/v1"
  api_key_env: "OPENROUTER_API_KEY"
runtime:
  image: "aibitat-harness-runtime:latest"
# Session transcripts (default shown). Resolved relative to this agent folder.
sessions: "./sessions"
safety:
  approval_required: false
options:
  supports_vision: false
  max_tool_iterations: 0
  command_timeout: 120
  max_context_tokens: 950000
---

You are a friendly conversational assistant. Answer user questions directly and clearly, without invoking tools or simulating tool use.
