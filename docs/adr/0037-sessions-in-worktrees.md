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

## Implementation (2026-11-09)

- `internal/gitdir`: a linked worktree's common git directory and main checkout, read from the files git leaves. A `.git` file counts only if it names a git directory `<common>/worktrees/<name>` (links resolved) whose `commondir` leads back to that `<common>` and whose `gitdir` names the `.git` file back. Files made in a folder (a `.git` file, and a folder beside it naming another repository, by an absolute path or by `..`) then lead only to that folder: they cannot make it a trusted repository's worktree, nor widen the git tools' sandbox (found by durability-reviewer: the first check took the `commondir` as given).
- `internal/runtime/worktree.go`: `MakeWorktree` (prunes worktrees whose folder is gone first; a branch `kairo/<name>` already there is checked out rather than made), `RemoveWorktree` (only a worktree in the data directory's `worktrees/`; refused while a session in it is working or has commands running; with changes, only with force; `git branch -d`, so an unmerged branch stays), `RepoSessions` (the repository's worktrees from `git worktree list`; sessions of worktrees removed since are listed as gone). A session whose worktree was removed is refused a turn (`ErrFolderGone`, 410).
- git runs in the daemon outside the sandbox, as the user's command (`internal/invariants`' list of files that start commands has `worktree.go`, as it has the plugins' git), and with `core.hooksPath=/dev/null`: making a worktree checks it out, and the repository's `post-checkout` hook would otherwise run outside the sandbox.
- One worktree is made at a time (a lock in the runtime), so two asked for at once with no name do not both take the same free one.
- A message sent just before its worktree is removed can start a turn in a folder that is gone: its tools fail, and nothing runs twice. Not guarded further.
- `SessionInfo` gained `Working`, `Repo`, `Worktree`, `Branch`, `Tree` and `Gone`. "Commits ahead" is counted from the main checkout's `HEAD`, not from the commit the worktree started at (which is not recorded).
- Calls: `POST /worktrees`, `POST /worktrees/remove`, `GET /sessions?repo=`; over ACP `_kairocode/repo_sessions`, `_kairocode/make_worktree`, `_kairocode/remove_worktree` (a worktree with changes is the error code -32011, so the terminal UI asks again).
- `kairo -worktree[=name]` and `kairocode run -worktree[=name]` (a flag that stands alone or takes `=name`, so `run -worktree "the request"` does not take the request for a name); `kairocode sessions -repo`. The chat asks about trust in the folder first, then makes the worktree.
- The terminal UI: `/new` (it did not exist) and `/new --worktree [name]`; `/sessions`, read again every 2 seconds while open (one timer per opening of the screen). Taking up a session or starting one in a worktree moves the screen's folder there (its files are read again for `@`). A worktree the terminal's own session is in is not removed from there. Editors open the worktree's folder as any folder; the agent has no `/new`.
- **Changed from the decision:** the crash test. Nothing of KairoCode's is written between making the worktree and the session, so what a crash can leave is git's state: `TestWorktreeMadeAgainAfterACrash` makes the worktree again from each (the branch made and not the worktree; the folder gone while git lists it; the worktree there), instead of killing a process.
- Tests: `TestLinked` (with forged files: an absolute `commondir`, `..`, `/`, and a folder laid out as git's; each was taken before the fix); `TestGitCommitInWorktree` (the git tools commit from a worktree inside the sandbox, bash cannot write the common git directory, and it fails without the change; the repository is made outside the temporary directories every command may write: in the package's folder where those are under `/tmp`); `TestWorktreeSession` (with removal refused while a session in it waits for an approval), `TestWorktreeKeepsUnmergedBranch`, `TestWorktreeMadeAgainAfterACrash`; `TestRunInAWorktree`; `TestRepoSessions` (the screen, removing with and without changes, taking up, `/new --worktree`).

## Alternatives considered

- **Worktrees inside the repository (`.kairocode/worktrees/<name>`, as Claude Code does).** Easy to find, but other tools walk into them, and every repository needs a `.gitignore` line.
- **Copies or clones instead of worktrees.** Independent, but slow for big repositories, and branches made in a clone are not in the main repository until pushed.
- **Sessions side by side in one folder, with locks on files.** No setup, but builds and tests still see each other's changes, and the changes mix in one diff.
- **Making the worktree as a step of the session workflow.** It would be recorded and resumed, but the session's folder must be known before the session starts (its tools, settings and trust depend on it), and a worktree is easy to take up again by its path.
