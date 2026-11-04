# 0030 Flow drafts and publishing: separating writing from making usable

- Status: Accepted (2026-10-27)
- Date: 2026-10-27
- Related: ADR 0003 (flows), ADR 0017 (try), ADR 0029 (run history)

## Context

When the model writes a flow with `flow_write`, it goes into `.kairocode/workflows/<name>.star` the moment it is allowed, and runs right away from `/flow <name>`, hooks and `kairocode run`. The review (does it match what was asked, what does it touch) and the try (ADR 0017) appear in the approval of this write.

So the stage of rewriting and trying a draft a few times and the stage of making it the version everyone uses are one step. A half-written flow could even run from a hook or CI.

## Decision

1. **Writing makes a draft.** `flow_write` writes to `.kairocode/workflows/drafts/<name>.star`.
   - A draft does not run from `/flow <name>`, hooks, an agent's `workflow:`, or `kairocode run`.
   - The check before writing (`flowchart.Check`) stays as before.
   - Writing a draft passes without asking by default (it does not run). The user's rules can change this.
2. **Publishing is `flow_publish`.** It puts the draft as is at `.kairocode/workflows/<name>.star`.
   - The content is taken from the draft file, and the model passes only the name (and the list of what was asked). The model cannot change the content when publishing.
   - The review and try that used to appear in the approval of `flow_write` now appear in the approval of `flow_publish`. By default, it asks every time.
   - Like `flow_write`, the write plans first, then compares and replaces (idempotent).
3. **The command stays a single `/flow`, split by options.**
   - `/flow <name> [arguments]`: runs the published version (as now).
   - `/flow <name> --draft [arguments]`: runs the draft as a try (ADR 0017: read-only tools run, others are not called and "what would have been passed" is kept). `ask()` asks the user.
   - `/flow <name> --publish`: asks the model to publish that draft (`flow_publish`). The review screen appears, and if allowed, it becomes the published version.
   - `kairocode run "/flow <name> --draft …"` is written the same way.
   - `/flow` (list) lists drafts with "(draft)" added.
4. **History (ADR 0029):** runs with `--draft` are kept as tries.
5. **Existing flows:** what is in `.kairocode/workflows/<name>.star` keeps running as is, as the published version. If there is no `drafts/`, there are simply no drafts.

Where this shows up in the implementation: `internal/tools/flowwrite.go` (the draft location), `internal/tools/flowpublish.go` (the new tool; effect type idempotent), `internal/runtime/permission.go` (review moved to `flow_publish`; default rules), `internal/workflows` (`FlowCall.Draft`, `FlowLoadInput.Draft`, draft runs are `Dry`), `internal/runtime/flow.go` (reading drafts, `/flow` options, the list), `internal/runtime/flownew.go` (instructions to the model).

Decided during implementation (the intent of the decision is unchanged):
- **Only the draft as it was when decided is published.** In the permission check of `flow_publish` (when the review is shown), the pair of that call and the hash of the draft at that time is recorded. When planning the publish, it is compared with the current draft, and if they differ, it does not publish and returns "the draft changed after the review; call flow_publish again". This is so that content no one has seen is not published, when `flow_write` and `flow_publish` are put side by side in the same reply, or when the draft is rewritten while waiting for approval. The record is on KairoCode's side (`kc_message` kind `reviewed`), and the workflow is not changed.
- The path of a write plan is accepted only if it exactly matches `.kairocode/workflows/<name>.star` or `.kairocode/workflows/drafts/<name>.star`. When reading a draft, too, if `.kairocode`, `workflows` or `drafts` is a link, it is not read.
- `flow_check` checks the draft if there is one, otherwise the published version.
- Tests: `TestNewFlow` (draft, `--draft`, `--publish`), `TestPublishOnlyWhatWasReviewed`, `TestFlowPublish`, `TestFlowWrite`, `TestTryFlow` and others (the try appears in the publish approval).
- Editing flows in the terminal UI (`/flow edit`) still changes the published version directly. That is an operation where the user edits it themselves, and is outside the scope of this ADR (`flow_write`, written by the model).

## Consequences

- A half-written flow does not really run from anywhere until it is published. A draft can be rewritten any number of times and tried with `--draft`.
- The review and try happen once, when publishing.
- Tests
  - `flow_write` writes to the draft, and the published version stays as is. A draft does not run with `/flow <name>`.
  - `/flow <name> --draft` runs the draft as a try, and does not call write tools.
  - `flow_publish` puts the draft's content into the published version, and the review and try appear in its approval.
  - The try before writing (ADR 0017) runs in the publish approval.

## Alternatives considered

- **Separate commands, like `/flow publish <name>` and `/flow draft <name>`:** at the user's wish, these became options of `/flow`. It also reduces clashes between flow names and reserved words.
- **Publish without going through the model:** reliable, but the publish review and try screens are built as approvals inside a turn. The content is taken from the draft file, so going through the model does not change what is published.
- **Put drafts in the data directory:** the repository stays clean, but other people and other machines cannot see them, and git cannot show diffs.
