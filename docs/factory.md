# KairoCode as a software factory

Status: draft, 2026-11-13. This is what KairoCode is to become, written down before the milestones that build it. Changes to it are agreed with the user, and the decisions it leads to are made in ADRs. It borrows words from writing on software factories, but it is KairoCode's own design, not a copy of any of them.

## 1. What KairoCode is to be

**A software factory you talk to.**

- **The conversation is the interface.** A person talks with KairoCode, as today:
  - they say what they want;
  - KairoCode answers, and asks when it needs them;
  - KairoCode reports what came of the work.
- **Behind the conversation, work goes down lines.**
  - A line is a row of stations. At each station a machine or a person works on the item.
  - KairoCode sends work down lines from the conversation, follows it, and brings back to the conversation only what needs the person.
- **Small work is still done in the conversation's own turns**, as today. The factory is for work worth sending down a line.
- **People decide what kind of factory it is** (the user, 2026-11-13).
  - KairoCode gives the parts: machines, gates, lines, stations for people, queues and limits.
  - KairoCode does not fix one way of building software. A factory for a solo side project and one for a team's payment service are different factories, and both are built from the same parts.
  - KairoCode may propose a line, a gate or an improvement. Nothing about the factory changes until a person agrees.

What KairoCode is today stays underneath: a coding agent that does not break when it crashes, waits or runs side by side (requirements, section 1). It is what makes the lines safe to run.

## 2. Words

| Word | What it is in KairoCode |
|---|---|
| **Conversation** | A session, in the terminal UI, an ACP editor or `kairocode run`. The only place a person needs to be. |
| **Work item** | One request sent down a line. It has its own worktree and branch (ADR 0037), budget, status and record, and it runs without holding the conversation's turn. |
| **Line** | The row of stations an item goes through, from request to a change ready to merge. **Standard lines** are declared in the project. **Lines made on the spot** are put together by KairoCode in the conversation for one request: shown to the person, recorded, and kept as a standard line if the person wants it. |
| **Station** | A place on a line. A machine or a person works there, and a gate may stand at its exit. |
| **Machine** | What does the work at a station:<ul><li>a **flow**: a written, reviewed, durable procedure (`.kairocode/workflows/<name>.star`; ADR 0003, 0017, 0030);</li><li>an **agent**: a turn with tools;</li><li>a **check**: a command that passes or fails (ADR 0042);</li><li>a **tool**: one operation, such as opening a PR.</li></ul>A flow is one kind of machine. It is not the line. |
| **Person** | Someone working at a station: they review, answer, decide or accept. They are reached through the conversation and need not watch the line. |
| **Gate** | The checks at a station's exit. The item moves on only when they pass, and the verdict is recorded (ADR 0042). |

## 3. Why lines run well on kairo

A line runs many long items unattended. Nothing on it may be redone or lost, and everything must be explainable afterwards. kairo gives each item that:

- **A line run is a workflow.** The line is recorded when the item enters (standard or made on the spot, invariant 2). Each station is a recorded child (invariant 8). A crash resumes the item at the station it was at, and a change, a push or a PR is not done twice (invariants 3 and 4).
- **Lines and machines can change while items are on them.** A flow's script is recorded when it starts, and workflow versions drain (kairo's ADR 0060). An item finishes on the line as it was when the item entered.
- **Waiting costs nothing.** A person's review hours later, a CI result, or an event from outside waits in kairo, not in a process (invariant 7). Many items can wait on people at once.
- **Stations side by side do not redo finished work.** Agents run in parallel in their own worktrees and resume only what was not done.
- **The record is the evidence.** Why an item passed a gate, who decided what, what each station cost: it can all be viewed and replayed without calling a model (ADR 0023). Trusting a line to run with fewer people becomes something to check, not something to hope.

## 4. Principles

1. **The model never decides alone that work is done.** A gate decides, or a person does. Work that was not checked is never shown as passed.
2. **The conversation is the interface.** People never have to operate lines, items or queues directly. KairoCode shows them in the conversation as what it is doing for the person, and asks only what needs the person.
3. **People decide what kind of factory it is.** Through the conversation, or by editing the files, they decide:

   | What | Where it is written |
   |---|---|
   | Which lines exist, and their stations | standard lines, in the project |
   | Which machines stand at each station | the line, and the flows |
   | Which checks there are, and where the gates stand | `checks` (ADR 0042), and the line |
   | Where a person must stand, and paths that always need one | the line, and the project's settings |
   | How much runs without a person reading it | the line, within what the project allows |
   | Budgets, limits, and how many items run at once | `limits` (ADR 0039), `rateLimits` (ADR 0040), and the line |
   | Whether KairoCode may put lines together on the spot, and from which machines | the project's settings |
   | Whether a result is accepted | the person, in the conversation |

   - All of it is in files in the repository (the project's) or in the user's own settings. It is reviewed like code.
   - It is recorded when an item starts (invariant 2).
   - Where the user's and the project's settings disagree, the stricter one holds, as with limits (ADR 0039).
   - KairoCode starts with no standard lines and nothing running unattended. Each factory is built up by its people. Example lines may be offered, but never installed unasked.
   - What KairoCode proposes is always a draft a person reviews:
     - a line put together on the spot;
     - a line kept from work that went well;
     - a better check after a failure.
4. **Lines grow from work.** A line made on the spot that went well becomes a standard line. A procedure that went well becomes a flow (ADR 0036). A failure becomes a better check or a better line, as a draft a person reviews.
5. **People stand where the risk is.** A line runs with fewer people only when its gates are cheap, hard to game and stable, and its record shows it. Some paths (auth, billing, a public API) always have a person, whatever the line.
6. **Everything is in the record.** If it is not in the record, the factory did not do it.
7. **Local first.** One binary, one daemon, the user's own machine and keys. A team's shared daemon and running without a process come later, on kairo (ADR 0012).

## 5. The way there

Each step is a milestone with its own ADR. Steps marked "kairo" wait for the user's work on kairo to settle.

1. **Gates** (M13, ADR 0042): checks decide when work is done. They are placed on a flow's agents and on a plain turn now, and on stations once lines exist.
2. **Lines and work items**:
   - Standard lines are declared in the project, and KairoCode puts lines together on the spot.
   - From the conversation, KairoCode sends items down them, each in its own worktree, running behind the conversation.
   - The conversation shows the items it started (status, station, cost), and what needs the person comes back into it.
   - A line made on the spot can be kept as a standard line.
3. **People at stations**:
   - Review stations: a person, a model, or both, by risk.
   - Paths that always need a person.
   - A line that keeps failing stops itself and tells the person.
   - A queue with a limit on items in progress per line.
4. **Results other programs read** (ADR 0041): exit codes and JSON, so schedules and CI can send work in and read what came out.
5. **Intake** (kairo): items from issues, tickets and alerts; waits on outside events; running without a process (ADR 0012).
6. **Feedback**: from an item that stopped or was reworked, a draft that improves the line, its checks or `AGENTS.md`.

Out of scope: shipping (deploying, canaries, feature flags). A line ends at a change ready to merge, or merged where the line says so.

## 6. Settled questions (ADR 0043)

- **Where standard lines are declared:** in files of their own, `.kairocode/lines/<name>.json`, in the project. A line is a list of stations, each with one machine and a gate; branches and loops belong in a flow, standing at a station.
- **Where items live:** each item is a session of its own, in a worktree of its own, pointing back to the conversation that started it; the conversation keeps its items. Not runs under the conversation, which would hold it as a flow does, and lack what a session has.
- **How KairoCode chooses:** the conversation's model picks a standard line and says which and why, or proposes one put together on the spot, which is always asked about; the person can always name one (`/line`).
