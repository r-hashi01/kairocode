# 0005 Run the processes KairoCode starts inside an OS sandbox modeled on Codex

- Status: Accepted (2026-10-09)
- Date: 2026-10-09
- Related: requirements FR-6, FR-7, FR-10, FR-12, NFR-7, NFR-8; design sections 3.2 and 4; ADR 0002 (permission rules), 0003 (workflows); invariants 4 and 6 in AGENTS.md

## Context

The bash tool's effect type is "real", and if its result is unknown, a person is asked (design section 3.2). If we protect only with permission rules (ADR 0002), the only way to make it safe is to add more "ask"s, which moves away from the goal of getting work done with the same effort as opencode.

Codex CLI puts the commands it runs in an OS sandbox, stopping writes outside the working directory and the network. The user approved of this direction (2026-10-09).

On 2026-10-09 we looked into the source of `github.com/openai/codex` main (commit `2351d9e1`). The paths below are relative to `https://github.com/openai/codex/blob/main/`.

- **macOS:**
  - It runs `/usr/bin/sandbox-exec -p <SBPL> -D<KEY>=<path>… -- <cmd>` (`codex-rs/sandboxing/src/seatbelt.rs`).
  - The policy starts from `(deny default)`, allows reading everything, and allows writing per writable root.
  - `.git`, `.agents`, `.codex` and `.aws` directly under a writable root are excluded from writing, and renaming and deleting their ancestors is stopped too.
  - Paths are not embedded in the string; they are passed as `-D` arguments.
- **Linux:**
  - It uses bubblewrap. On top of `--ro-bind / /`, it `--bind`s the writable roots and makes `.git` read-only again. The namespaces are `--unshare-user` and `--unshare-pid`, and `--unshare-net` when the network is stopped.
  - It also starts itself again as a helper process (`codex-linux-sandbox`) and applies `PR_SET_NO_NEW_PRIVS` and seccomp.
  - Landlock is treated as the old path.
- **Modes:** three: `read-only`, `workspace-write` and `danger-full-access`. The writable roots of workspace-write are the working directory, `/tmp`, `$TMPDIR` and those added in the configuration. The network is stopped by default.
- **Escalation:** the model can ask to run outside the sandbox through the tool's arguments (`sandbox_permissions`, `justification`). A failure refused by the sandbox is told apart by guessing from the exit code (SIGSYS on Linux) and words in the output.
- **Environment variables:** the child process's environment variables are first emptied and then rebuilt by a policy.
- **License:** Apache-2.0. The base policy comes from Chromium (BSD).
- **Known problems:**
  - GHSA-w5fx-fh39-j5rw: it treated the `cwd` passed by the model as a writable root, so it could write outside the working directory.
  - bubblewrap does not run because of the userns restriction by AppArmor on Ubuntu 24.04.
  - Apple has deprecated `sandbox-exec`.

## Decision

### What it covers

- Every command KairoCode starts runs inside the sandbox. This covers the bash tool, a workflow's `tool("bash", …)` (ADR 0003), and a command's `` !`shell` `` (ADR 0002).
- Two things are not covered. Both are set up by the user.
  - LSP servers (which keep running for a long time)
  - MCP servers

### Modes

| Mode | Writes | Network | Where it is used |
|---|---|---|---|
| `read-only` | none | none | plan mode, agents with `effect_limit: read` |
| `workspace-write` | the working directory, `/tmp`, `$TMPDIR`, roots added in the configuration | none (can be opened in the configuration) | build mode (default) |
| `full-access` | no limit | yes | only commands whose escalation was approved |

- **The writable root is fixed to the working directory at the time the session started.** The `cwd` the model passes is used only when it can be normalized to a path inside it (the lesson of GHSA-w5fx-fh39-j5rw). A root with a symbolic link along its path is refused.
- **What is protected from writes:** writes to `.git`, `.kairocode`, `.opencode`, `.claude` and `.agents` directly under a writable root are stopped. Renaming and deleting their ancestors is stopped too.
  - KairoCode's hooks and permission settings (ADR 0002, 0003) live here. This is so that a command inside the sandbox cannot rewrite its own permissions or the hooks that run automatically.
  - Operations that change the contents of `.git` (commit and so on) are done with the git tools (the idempotent git operations of design section 3).

### Relation to effect types

- A command inside the `read-only` sandbox can neither write nor use the network. So its effect type can be treated as `tools.Read`. Even if its result is unknown, it may be retried, and no person needs to be asked.
- A command inside `workspace-write` is still `tools.Real`, as before. If its result is unknown, it stops at blocked (needs confirmation) (invariant 4). The sandbox limits the reach of the effect to the working directory; it does not guarantee that nothing happens twice.

### Relation to permission rules

- The permission rules (ADR 0002) are still decided first, as before. The sandbox limits the extent of damage after that.
- The default rules are relaxed on the assumption of the sandbox.
  - bash for the build agent is `allow` when inside the sandbox.
  - Escalation (running outside the sandbox) is always `ask`, whatever the rules.
- **The escalation flow:**
  - The model passes `sandbox: "escalate"` and a reason (`justification`) in the bash tool's arguments.
  - KairoCode asks the user for approval with the reason attached (`kairo.WaitFor`; `session/request_permission` in ACP).
  - If approved, it runs once with `full-access`. There is also an option to record the approval as "allow this shape of command from now on".
- **A failure refused by the sandbox:**
  - Guessing from the exit code and the output, a sentence suggesting escalation is added to the result for the model.
  - It is not automatically run again outside the sandbox.

### The mechanism on each OS

- **macOS:**
  - `/usr/bin/sandbox-exec` is used by its absolute path.
  - The SBPL is based on Codex's policies (`seatbelt_base_policy.sbpl`, `seatbelt_network_policy.sbpl` and others). It goes in `third_party/codex-sandbox/` with attribution and a clear statement of changes (Apache-2.0 and Chromium's BSD).
  - Paths are passed as `-D` arguments.
- **Linux:**
  - bubblewrap builds the namespaces and the file system.
  - It starts itself again as a helper process (`kairocode __sandbox`; the role is switched by an argument), applies `PR_SET_NO_NEW_PRIVS` and seccomp, and then execs the target command.
  - The seccomp filter is built with a pure Go library that needs no cgo (`github.com/elastic/go-seccomp-bpf`), to distribute a single binary (ADR 0001).
- **When the sandbox cannot be used (fail-closed):** cases such as no bubblewrap, userns not available (AppArmor, containers), or no `sandbox-exec`.
  - **It does not silently fall back to running outside the sandbox.** It tells the user at startup, and bash becomes `ask` every time.
  - For environments isolated from the outside, such as containers, there is a setting `sandbox: external`. Only when it is chosen explicitly does it run without the sandbox.

### Environment variables

- The child process's environment variables are first emptied and then rebuilt.
- Those whose names contain `KEY`, `SECRET`, `TOKEN` or `PASSWORD` are **removed by default** (safer than Codex's default; invariant 6). The setting `env.include` can pass them by name.
- `KAIROCODE_SANDBOX=<mode>` is set. When the network is stopped, `KAIROCODE_SANDBOX_NETWORK_DISABLED=1` is set too.

### What this ADR does not decide

- A network allowlist (a proxy). A mechanism like Codex's `network-proxy` will be decided in a separate ADR when needed.
- Isolating workflow scripts. Workflows are written in Starlark (ADR 0003), so they are isolated by the language. No OS sandbox is needed. `tool("bash", …)` called from a workflow goes through this ADR's sandbox.

### Addendum (2026-10-09, what was settled while implementing M1)

- **`kairocode.json` was added to the protected names.** The project's permission rules (ADR 0002) are here. If a command inside the sandbox rewrote it, it could widen its own permissions from the next turn on.
- **When writable roots are nested:** when the working directory was under `$TMPDIR`, the allow rule for `$TMPDIR` made the working directory's `.git` writable (found in a macOS test). From each root's allow rule, the protected names of every root below it are excluded.
- **Linux:** the shape of starting itself again as a helper process was dropped. A filter built with `go-seccomp-bpf` is passed directly to bubblewrap's `--seccomp <fd>`. What is refused is `ptrace`, `process_vm_readv`, `process_vm_writev` and `io_uring_*` (the same as Codex). The network is cut with `--unshare-net`.
- **Dependencies (added 2026-10-28):** the filter's prefix (checking the arch, refusing Unix sockets) is assembled with `golang.org/x/net/bpf` instructions and joined to the `go-seccomp-bpf` filter with `bpf.Assemble`. `golang.org/x/net` is a dependency `go-seccomp-bpf` already uses, and we import it directly (since 2026-10-09). Only the Linux part of `internal/sandbox` uses it.
- **A known limit on Linux:** it cannot prevent a protected name that does not exist yet (for example `.kairocode`) from being created. Binding something read-only requires the name to exist. Putting a read-only thing at the place of a name that does not exist leaves an empty directory on the host side (an empty `.git` confuses git). Protected names that exist can be protected. On macOS, names that do not exist cannot be created either. Codex has the same unresolved issue (#40685).
- **When `kairocode.json` is absent on Linux:** if there is no `kairocode.json` directly under the project (a common case), bash inside the sandbox can create it and widen its own permissions from the next turn on. This is the highest-impact case of the limit above. The write and edit tools refuse this name. For bash, the remedy will be decided when the user uses it on Linux (for example, not reading a `kairocode.json` created at the start of a turn).
- **write, edit and the git tools (2026-10-09, M1's review):** write and edit also refuse the protected names. `git_push`, which runs outside the sandbox, does not run the repository's hooks (`-c core.hooksPath=/dev/null push --no-verify`). A check (`internal/invariants`) confirms that the ways of starting a command outside the sandbox (`exec.Command`, building an `exec.Cmd` directly, `os.StartProcess`, `sandbox.FullAccess`) are not used outside designated files.
- **Tests:** they pass on both macOS and Linux (the Docker `golang:1.27` image with bubblewrap installed, run with `--privileged`). There is no CI, so the Linux tests are run by hand.

## Consequences

- `github.com/elastic/go-seccomp-bpf` is added as a dependency.
- Codex's SBPL, its license and a NOTICE go in `third_party/codex-sandbox/`.
- One invariant is added (AGENTS.md): **"Commands KairoCode starts run inside the sandbox. They run outside the sandbox only for an escalation the user approved and for an explicit `sandbox: external`."** Code that starts commands with `os/exec` without the sandbox is not allowed outside `internal/sandbox`. A check is added to `internal/invariants`.
- Tests (added to M1's acceptance criteria; run on both macOS and Linux):
  - `TestSandboxDeniesWriteOutsideWorkspace`, `TestSandboxProtectsGitAndConfig` (writes to `.git`, `.kairocode` and so on, and renaming them, are refused).
  - `TestSandboxDeniesNetwork`, `TestSandboxStripsSecrets`.
  - `TestSandboxIgnoresModelCwdOutsideRoot` (the same shape as GHSA-w5fx-fh39-j5rw).
  - `TestSandboxFailsClosed` (when the sandbox cannot be used, bash becomes `ask` every time).
  - `TestEscalationAlwaysAsks`.
- On CI's Linux runners, the userns restriction (AppArmor on Ubuntu 24.04) has to be lifted in the configuration. For environments where it cannot be lifted, the tests check the fail-closed side.
- Apple has deprecated `sandbox-exec`. If it goes away, isolation on macOS will have to be redone. Codex is in the same position.

## Alternatives considered

- **Protect only with permission rules:** the only way to make it safe is to add more "ask"s, which adds effort. It also cannot prevent slipping past the rules (other ways of writing the same effect).
- **Call Codex's `codex sandbox` as an outside command:** no implementation is needed, but Codex has to be installed, and this CLI has no stability guarantee.
- **Mainly use Landlock on Linux:** Codex also treats it as the old path. Isolation of the network and Unix sockets is not enough.
- **Run inside a container (Docker and so on):** the isolation is strong, but it needs Docker and starts slower. It is too heavy for a tool used locally. Using KairoCode inside a container is supported with `sandbox: external`.
