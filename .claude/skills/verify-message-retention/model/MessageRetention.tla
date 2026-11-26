-------------------------- MODULE MessageRetention --------------------------
(***************************************************************************)
(* The message part of the storage-retention sweep (ADR 0045) and the      *)
(* writers that run beside it: sending, history.extend, closing, failing,  *)
(* forking, and kairo removing a finished tree. Each action is one step of *)
(* the code; ../SKILL.md (coverage) maps each to its function.             *)
(*                                                                         *)
(* Property (NothingReachedIsLost): no message is deleted from kc_message  *)
(* while a run kairo holds names it, directly or through the bodies of the *)
(* messages it names, or while a writer holds its hash to name next.       *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences, TLC

CONSTANTS
    Sessions,   \* session runs (s/<id>), none started at first
    Texts,      \* what a person types: the same text is the same message
    MaxTurns,   \* turns a session takes at most
    Grace,      \* retention.grace, in ticks of the clock
    Keep,       \* kairo's KeepFinished, in ticks (shorter than the grace)
    MaxTime,    \* the clock stops here
    \* The two findings (SKILL.md), each off unless check.sh -set turns it on:
    RemoveDuringFork, \* F1: kairo may remove a fork's source while Fork runs
    ForkFailed        \* F2: Fork may start from a failed session

ASSUME Keep < Grace /\ Grace < MaxTime /\ {RemoveDuringFork, ForkFailed} \subseteq BOOLEAN

(* kc_message is keyed by its content's hash, so here a message is its     *)
(* content. Every message has the same four fields, so TLC compares them.  *)

\* A user message: Send's store.PutMessage (kind message).
Msg(t) == [kind |-> "message", text |-> t, prev |-> {}, msgs |-> {}]
\* A history link: store.Extend's Link{Prev, Messages} (kind history).
Link(prev, m) == [kind |-> "history", text |-> "", prev |-> prev, msgs |-> {m}]
\* The hashes a body names: every sha256: in it (hashRe).
Body(h) == h.prev \cup h.msgs

\* What hs reach through the bodies of the messages among st: the mark of
\* sweepMessages, where Store.Get of a hash not found is passed over.
RECURSIVE Mark(_, _)
Mark(hs, st) ==
    LET next == hs \cup UNION {Body(h) : h \in hs \cap st}
    IN IF next = hs THEN hs ELSE Mark(next, st)

\* What hs reach through bodies, whether stored or not.
RECURSIVE Reach(_)
Reach(hs) ==
    LET next == hs \cup UNION {Body(h) : h \in hs}
    IN IF next = hs THEN hs ELSE Reach(next)

VARIABLES
    now,      \* the clock (time.Now), in ticks
    stored,   \* the hashes kc_message holds
    at,       \* kc_message.at: when each was last written
    status,   \* each session's run: "none", "live", "closed", "failed", "removed"
    endedAt,  \* when it finished (kairo's updated_at)
    input,    \* its SessionInput.History: {} or {hash}
    output,   \* its SessionOutput.Head, once closed: {} or {hash}
    named,    \* the hashes the records of the runs under it name
    head,     \* runSession's head: {} or {hash}
    heads,    \* the Head of each of its turns that completed, in order
    turn,     \* its turn in progress
    fork,     \* a Fork between reading its source and creating the session
    sw        \* the sweep in progress

vars == <<now, stored, at, status, endedAt, input, output, named, head, heads, turn, fork, sw>>
kairo == <<status, endedAt, input, output, named, head, heads>>

\* A run kairo holds (K.Get finds it): live, or finished and not yet removed.
Held == {"live", "closed", "failed"}

NoTurn == [ph |-> "idle", msg |-> {}, link |-> {}]
NoSweep == [ph |-> "idle", grace |-> 0, todo |-> {}, names |-> {}, marked |-> {}, unswept |-> {}]

Init ==
    /\ now = 0
    /\ stored = {}
    /\ at = [h \in {} |-> 0]
    /\ status = [s \in Sessions |-> "none"]
    /\ endedAt = [s \in Sessions |-> 0]
    /\ input = [s \in Sessions |-> {}]
    /\ output = [s \in Sessions |-> {}]
    /\ named = [s \in Sessions |-> {}]
    /\ head = [s \in Sessions |-> {}]
    /\ heads = [s \in Sessions |-> <<>>]
    /\ turn = [s \in Sessions |-> NoTurn]
    /\ fork = {}
    /\ sw = NoSweep

\* Store.Put: INSERT ... ON CONFLICT (hash) DO UPDATE SET at = excluded.at.
\* Written again, a message keeps its body and its time moves on.
Put(h) ==
    /\ stored' = stored \cup {h}
    /\ at' = [x \in stored \cup {h} |-> IF x = h THEN now ELSE at[x]]

-----------------------------------------------------------------------------
(* The writers. *)

\* CreateSession (send.go), a new conversation: no history.
Create(s) ==
    /\ status[s] = "none"
    /\ \A f \in fork : f.to # s
    /\ status' = [status EXCEPT ![s] = "live"]
    /\ UNCHANGED <<now, stored, at, endedAt, input, output, named, head, heads, turn, fork, sw>>

\* Send (send.go): store.PutMessage, then the signal; runSession starts the
\* turn, whose TurnInput names History (head) and Message. Between the put
\* and the record the message is fresh: the grace keeps it.
Send(s, t) ==
    /\ status[s] = "live"
    /\ turn[s].ph = "idle"
    /\ Len(heads[s]) < MaxTurns
    /\ Put(Msg(t))
    /\ turn' = [turn EXCEPT ![s] = [ph |-> "running", msg |-> {Msg(t)}, link |-> {}]]
    /\ named' = [named EXCEPT ![s] = @ \cup head[s] \cup {Msg(t)}]
    /\ UNCHANGED <<now, status, endedAt, input, output, head, heads, fork, sw>>

\* The history.extend action (declare.go): store.Extend puts
\* Link{Prev: History, Messages}. Its output is not recorded yet.
Extend(s) ==
    /\ turn[s].ph = "running"
    /\ LET l == Link(head[s], CHOOSE m \in turn[s].msg : TRUE)
       IN /\ Put(l)
          /\ turn' = [turn EXCEPT ![s] = [ph |-> "extended", msg |-> turn[s].msg, link |-> {l}]]
    /\ UNCHANGED <<now, status, endedAt, input, output, named, head, heads, fork, sw>>

\* The call's output is recorded, the turn completes with it as its Head
\* (TurnOutput.Head), and runSession takes it as head.
TurnEnd(s) ==
    /\ turn[s].ph = "extended"
    /\ LET l == CHOOSE x \in turn[s].link : TRUE
       IN /\ named' = [named EXCEPT ![s] = @ \cup {l}]
          /\ head' = [head EXCEPT ![s] = {l}]
          /\ heads' = [heads EXCEPT ![s] = Append(@, l)]
    /\ turn' = [turn EXCEPT ![s] = NoTurn]
    /\ UNCHANGED <<now, stored, at, status, endedAt, input, output, fork, sw>>

\* CloseSession: EventClose, and runSession returns SessionOutput{Head}.
Close(s) ==
    /\ status[s] = "live"
    /\ turn[s].ph = "idle"
    /\ status' = [status EXCEPT ![s] = "closed"]
    /\ output' = [output EXCEPT ![s] = head[s]]
    /\ endedAt' = [endedAt EXCEPT ![s] = now]
    /\ UNCHANGED <<now, stored, at, input, named, head, heads, turn, fork, sw>>

\* runSession returns an error that is no turn's outcome (a call of its own
\* failed for good): kairo records the run failed, with no output.
Fail(s) ==
    /\ status[s] = "live"
    /\ turn[s].ph = "idle"
    /\ status' = [status EXCEPT ![s] = "failed"]
    /\ endedAt' = [endedAt EXCEPT ![s] = now]
    /\ UNCHANGED <<now, stored, at, input, output, named, head, heads, turn, fork, sw>>

\* kairo's tick: RemoveFinished deletes, in one transaction, the tree of a
\* root finished before now - KeepFinished. K.Get and K.Children find
\* nothing of it after. Nothing in KairoCode holds it back while a Fork
\* reads it; RemoveDuringFork = FALSE assumes so (finding F1).
Remove(s) ==
    /\ status[s] \in {"closed", "failed"}
    /\ endedAt[s] + Keep < now
    /\ RemoveDuringFork \/ \A f \in fork : f.from # s
    /\ status' = [status EXCEPT ![s] = "removed"]
    /\ input' = [input EXCEPT ![s] = {}]
    /\ output' = [output EXCEPT ![s] = {}]
    /\ named' = [named EXCEPT ![s] = {}]
    /\ head' = [head EXCEPT ![s] = {}]
    /\ heads' = [heads EXCEPT ![s] = <<>>]
    /\ UNCHANGED <<now, stored, at, endedAt, turn, fork, sw>>

\* Fork (fork.go): K.Get(from) and K.Children(from), then the Head of one
\* of its completed turns. Nothing is put: the hash is held as it is. Fork
\* takes any session kairo holds; ForkFailed = FALSE leaves out the failed
\* ones (finding F2).
ForkRead(from, to) ==
    /\ fork = {}
    /\ status[from] \in (IF ForkFailed THEN Held ELSE {"live", "closed"})
    /\ status[to] = "none"
    /\ to # from
    /\ \E i \in 1..Len(heads[from]) :
         fork' = {[from |-> from, to |-> to, h |-> heads[from][i]]}
    /\ UNCHANGED <<now, stored, at, kairo, turn, sw>>

\* Fork: CreateSession(to, SessionInput{History: tout.Head}).
ForkCreate ==
    /\ fork # {}
    /\ LET f == CHOOSE x \in fork : TRUE
       IN /\ status' = [status EXCEPT ![f.to] = "live"]
          /\ input' = [input EXCEPT ![f.to] = {f.h}]
          /\ head' = [head EXCEPT ![f.to] = {f.h}]
    /\ fork' = {}
    /\ UNCHANGED <<now, stored, at, endedAt, output, named, heads, turn, sw>>

-----------------------------------------------------------------------------
(* The sweep (sweep.go): sweepRoots, then sweepMessages. sweepMu lets one *)
(* run at a time.                                                         *)

\* sweep: grace = now - retention.grace; sweepRoots' one K.List of every
\* root, each with the Input and Output it has then (take).
List ==
    /\ sw.ph = "idle"
    /\ LET roots == {s \in Sessions : status[s] \in Held}
       IN sw' = [ph |-> "walk", grace |-> now - Grace, todo |-> roots,
                 names |-> UNION {input[s] \cup output[s] : s \in roots},
                 marked |-> {}, unswept |-> {}]
    /\ UNCHANGED <<now, stored, at, kairo, turn, fork>>

\* walk: K.Children of one root, and the runs under it, each with its
\* Input and Output. Of a tree kairo removed meanwhile there are none.
Walk(s) ==
    /\ sw.ph = "walk"
    /\ s \in sw.todo
    /\ sw' = [sw EXCEPT !.todo = @ \ {s},
                        !.names = @ \cup (IF status[s] \in Held THEN named[s] ELSE {})]
    /\ UNCHANGED <<now, stored, at, kairo, turn, fork>>

\* sweepMessages: the mark, from the roots through each reached message's
\* body. Only the sweep deletes, and bodies never change, so the loop of
\* Store.Get is one step here.
MarkStep ==
    /\ sw.ph = "walk"
    /\ sw.todo = {}
    /\ sw' = [sw EXCEPT !.ph = "unswept", !.marked = Mark(sw.names, stored)]
    /\ UNCHANGED <<now, stored, at, kairo, turn, fork>>

\* Store.Unswept: SELECT hash FROM kc_message WHERE at < grace.
Unswept ==
    /\ sw.ph = "unswept"
    /\ sw' = [sw EXCEPT !.ph = "delete", !.unswept = {h \in stored : at[h] < sw.grace}]
    /\ UNCHANGED <<now, stored, at, kairo, turn, fork>>

\* One hash of Unswept: passed over if marked, else Store.SweepMessage:
\* DELETE FROM kc_message WHERE hash = ? AND at < grace.
SweepMessage(h) ==
    /\ sw.ph = "delete"
    /\ h \in sw.unswept
    /\ sw' = [sw EXCEPT !.unswept = @ \ {h}]
    /\ IF h \notin sw.marked /\ h \in stored /\ at[h] < sw.grace
         THEN /\ stored' = stored \ {h}
              /\ at' = [x \in stored \ {h} |-> at[x]]
         ELSE UNCHANGED <<stored, at>>
    /\ UNCHANGED <<now, kairo, turn, fork>>

\* The messages are done (the sweep's other parts are not modelled).
Swept ==
    /\ sw.ph = "delete"
    /\ sw.unswept = {}
    /\ sw' = NoSweep
    /\ UNCHANGED <<now, stored, at, kairo, turn, fork>>

-----------------------------------------------------------------------------
(* Time and crashes. *)

\* Days pass between operations, not inside one: a sweep, a fork, and a put
\* before the record that names it each take moments.
Tick ==
    /\ now < MaxTime
    /\ sw.ph = "idle"
    /\ fork = {}
    /\ \A s \in Sessions : turn[s].ph # "extended"
    /\ now' = now + 1
    /\ UNCHANGED <<stored, at, kairo, turn, fork, sw>>

\* The daemon stops. The sweep and a fork in progress are lost (a sweep cut
\* off begins again from the start); a history.extend not recorded runs
\* again on resume. kairo's records and kc_message stay.
Crash ==
    /\ sw.ph # "idle" \/ fork # {} \/ \E s \in Sessions : turn[s].ph = "extended"
    /\ sw' = NoSweep
    /\ fork' = {}
    /\ turn' = [s \in Sessions |->
                  IF turn[s].ph = "extended"
                  THEN [ph |-> "running", msg |-> turn[s].msg, link |-> {}]
                  ELSE turn[s]]
    /\ UNCHANGED <<now, stored, at, kairo>>

Next ==
    \/ \E s \in Sessions :
         \/ Create(s) \/ Extend(s) \/ TurnEnd(s) \/ Close(s) \/ Fail(s) \/ Remove(s)
         \/ Walk(s)
         \/ \E t \in Texts : Send(s, t)
         \/ \E to \in Sessions : ForkRead(s, to)
    \/ ForkCreate
    \/ List \/ MarkStep \/ Unswept \/ Swept
    \/ \E h \in sw.unswept : SweepMessage(h)
    \/ Tick \/ Crash

Spec == Init /\ [][Next]_vars

\* Sessions are alike, and so are texts: TLC checks one of each permutation.
Symmetry == Permutations(Sessions) \cup Permutations(Texts)

-----------------------------------------------------------------------------
(* Invariants. *)

\* What the runs kairo holds name, and what a writer holds to name next:
\* a fork's head between reading and creating, a link put and not recorded.
Named ==
    UNION {input[s] \cup output[s] \cup named[s] : s \in {x \in Sessions : status[x] \in Held}}
    \cup {f.h : f \in fork}
    \cup UNION {turn[s].link : s \in Sessions}

\* The property: everything they reach is still in kc_message.
NothingReachedIsLost == Reach(Named) \subseteq stored

TypeOK ==
    /\ now \in 0..MaxTime
    /\ DOMAIN at = stored
    /\ \A h \in stored : at[h] \in 0..now
    /\ status \in [Sessions -> {"none", "live", "closed", "failed", "removed"}]
    /\ \A s \in Sessions : Cardinality(head[s]) <= 1 /\ Cardinality(input[s]) <= 1
    /\ Cardinality(fork) <= 1
    /\ sw.ph \in {"idle", "walk", "unswept", "delete"}
=============================================================================
