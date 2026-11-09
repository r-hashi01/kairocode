# 0037 Sessions side by side in git worktrees, and a screen listing them

- Status: Accepted (2026-11-09)
- Date: 2026-11-09
- Related: ADR 0005 (sandbox), ADR 0008 (one daemon per data directory), ADR 0010 (terminal UI, trust), ADR 0028 (background commands), invariants 2 and 8, `docs/milestones.md` M9

## Context

kairo runs sessions side by side already, and the daemon serves any number of them. But two sessions in the same folder edit the same files: one's `go test` sees the other's half-written change, and their changes mix in one diff. To work on two things at once, a person makes a second checkout by hand.

A git worktree is that second checkout, sharing the repository's objects and branches. Claude Code and Codex both start sessions in worktrees for this reason.

A session's folder is fixed when it starts (`SessionInput.Dir`), and every tool works inside it. So a session in a worktree is an ordinary session whose folder is the worktree: what is missing is making the worktree, letting git work in it, and seeing the sessions of a repository together.

## Decision

1. **Starting a session in a new worktree:** `kairo --worktree [name]`, `kairocode run --worktree [name] …`, and `/new --worktree [name]` in the terminal UI (`/new` is new: it starts another session in this terminal; with `--worktree`, in a new worktree).
   - The client asks the daemon, which runs `git worktree add -b kairo/<name> <path> HEAD` in the folder's repository, then creates the session with the worktree as its folder.
   - The name is given, or the first free of `1`, `2`, … in that repository. The branch is `kairo/<name>`, from the folder's current `HEAD`. Uncommitted changes in the folder are not carried over; the command says so when there are any.
   - The worktree lives outside the repository, in the data directory: `<data>/worktrees/<repository folder's name>-<first 8 hex of sha256(its path)>/<name>`. Inside the repository it would be seen by other tools' file walks (test runners, bundlers, grep without `.gitignore`).
   - It is not a step of a workflow: like choosing a folder, it happens before the session starts. Run again after a crash, it takes up a worktree already there at that path on that branch.
   - Editors (ACP) open folders themselves; a worktree made here can be opened in one like any folder.
2. **Trust:** a worktree of a trusted repository is trusted (the trust of `git rev-parse --git-common-dir`'s repository, ADR 0010).
3. **git in a worktree:** the git tools (`AllowGit`, ADR 0005) may also write the repository's common git directory (objects, refs, `worktrees/<name>`), which is outside the worktree. Other commands do not: `.git` stays protected as now.
4. **Settings:** the project's settings (`kairocode.json`, `.kairocode`, `.opencode`, `AGENTS.md`) are those of the worktree's checkout. Files git does not track (`.env`, built dependencies) are not there; a worktree that needs them is set up by the person, or by the session.
5. **The screen listing them:** `/sessions` in the terminal UI, and `kairocode sessions --repo`, list the sessions of the repository across all its worktrees, the main checkout among them.
   - Each row: the folder (`main`, or the worktree's name) and branch; the state (working, waiting for you (with the count), idle, closed; background commands, ADR 0028); the first message; when it was last used; and the worktree's changes (files changed and lines, uncommitted included; commits ahead of the branch it started from).
   - The screen is refreshed while it is open. That is a screen being drawn, not a wait inside a workflow (invariant 7 is about the latter).
   - Enter takes up the chosen session in this terminal, as `/resume` does; the terminal's folder becomes the session's.
   - `d` removes a worktree whose sessions are closed or idle: `git worktree remove`, then `git branch -d kairo/<name>` (kept if not merged, and the screen says so). With uncommitted changes, it asks first. The sessions' records stay; their folder is shown as removed, and they take no more turns.
   - Merging a worktree's branch is left to git, or to a session asked to do it.
6. **The daemon's API:** `SessionInfo` gains `Repo` (the main checkout's folder) and `Branch`; listing takes a repository as well as a folder. Making and removing worktrees are calls of their own.
7. **Workflows:** no change. The session's folder is recorded as now, and run IDs follow as now (invariant 8).

Where this shows in the implementation: `internal/runtime/worktree.go` (making, finding and removing worktrees, the repository of a folder), `internal/runtime/sessions.go` (`Repo`, `Branch`, changes, listing by repository), `internal/tools/git.go` and `internal/sandbox` (the common git directory writable for the git tools), `internal/api`, `internal/server`, `internal/daemon` (the calls), `internal/cli` (`--worktree`, `sessions --repo`), `internal/tui` (`/new --worktree`, `/sessions`), `internal/trust` (trust by repository).

## Consequences

- Two or more sessions work on one repository at once without seeing each other's half-done changes, each on its own branch, and one screen shows what each is doing and which waits for a person.
- Worktrees take disk space in the data directory until removed.
- Untracked files are not in a new worktree, which surprises when a project needs `.env` or installed dependencies.
- Tests (to add):
  - `TestWorktreeSession`: `--worktree` makes a worktree on `kairo/<name>` from `HEAD` and a session in it; a write there does not change the main checkout; a second call with the same name takes it up.
  - `TestCrashWorktreeSession`: killed after the worktree is made and before the session is: started again, it takes up the worktree and makes one session.
  - The git tools can commit in a worktree inside the sandbox; `bash` cannot write the common git directory.
  - `sessions --repo` lists the main checkout's and the worktrees' sessions with branch, state and changes.
  - Removing: refused while a session is working; asks with uncommitted changes; keeps an unmerged branch.

## Alternatives considered

- **Worktrees inside the repository (`.kairocode/worktrees/<name>`, as Claude Code does).** Easy to find, but other tools walk into them, and every repository needs a `.gitignore` line.
- **Copies or clones instead of worktrees.** Independent, but slow for big repositories, and branches made in a clone are not in the main repository until pushed.
- **Sessions side by side in one folder, with locks on files.** No setup, but builds and tests still see each other's changes, and the changes mix in one diff.
- **Making the worktree as a step of the session workflow.** It would be recorded and resumed, but the session's folder must be known before the session starts (its tools, settings and trust depend on it), and a worktree is easy to take up again by its path.
