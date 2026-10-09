# Codex sandbox policies

`internal/sandbox/seatbelt_base_policy.sbpl` and
`internal/sandbox/seatbelt_network_policy.sbpl` are copied unchanged from
OpenAI Codex (https://github.com/openai/codex, `codex-rs/sandboxing/src/`,
main at commit 2351d9e1, 2026-10-09), under the Apache License 2.0 in this
directory (`LICENSE`, `NOTICE`). The base policy is itself derived from
Chromium's sandbox policy (BSD-style licence), as its comments say.

KairoCode's own code (`internal/sandbox/seatbelt.go`) writes the rest of the
policy, after the way Codex's `seatbelt.rs` does it, with these changes: the
protected names are `.git`, `.kairocode`, `.opencode`, `.claude` and
`.agents` (ADR 0005); there is no network proxy; and paths are passed as
`-D` parameters, never written into the policy text.
