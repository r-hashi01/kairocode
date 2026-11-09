package api

import (
	"errors"
)

import ()

// ErrQuestion: a flow's question was given a yes or no, or an answer that
// is not one of its options.
var ErrQuestion = errors.New("not an answer to this question")

// ErrAnswered: the request was answered already (by another client), or
// waits no more.
var ErrAnswered = errors.New("already answered, or no longer waiting")

// ErrNoCommand: the session has no command of that name.
var ErrNoCommand = errors.New("no such command")

// ErrNoFlowName: /flow was given no name.
var ErrNoFlowName = errors.New("/flow needs a flow's name: /flow <name> [what to give it]")

// ErrNoFlowRequest: /flow new was given nothing to make a flow of.
var ErrNoFlowRequest = errors.New("/flow new needs what the flow is to do: /flow new <what it does>")

// ErrNoFlowSource: /flow new --from-session named no session of the
// folder, or one with nothing done yet (ADR 0036).
var ErrNoFlowSource = errors.New("/flow new --from-session takes a session of this folder that has done something (kairocode sessions lists them)")

// ErrNoFlowRun: no flow's run of that id, running or kept.
var ErrNoFlowRun = errors.New("no flow run of that id (kept runs: kairocode flow runs)")

// ErrNotAFlow: a request to try what is not a flow's write; ErrTrying:
// one while a try of it is under way.
var (
	ErrNotAFlow = errors.New("this call writes no flow to try")
	ErrTrying   = errors.New("it is being tried: answer it, or wait for the try to end")
)

// ErrNoFlow: the session has run no flow.
var ErrNoFlow = errors.New("this session has run no flow")

// ErrNoForkPoint: the session has no finished turn to fork from there.
// ErrNoSnapshot: there is no record of the files after it, to put back.
// ErrForkWhileRunning: the files are not put back while a turn of the
// session runs in them.
var (
	ErrNoForkPoint      = errors.New("no finished turn to fork from")
	ErrNoSnapshot       = errors.New("there is no snapshot of the files after that turn, to put them back as they were")
	ErrForkWhileRunning = errors.New("a turn of the session is running in its files: fork with --files once it ends")
)

// ErrBadImage: an image is not taken (its kind, its size, how many).
var ErrBadImage = errors.New("image not taken")

// ErrClosing: the runtime is closing; what it is told, the next is to be.
var ErrClosing = errors.New("the daemon is closing: tell the next")

// ErrNoProvider: no such provider. ErrNotInstalled: an agent of the
// registry not installed (yet).
var (
	ErrNoProvider   = errors.New("no such provider")
	ErrNotInstalled = errors.New("not installed")
)

// ErrUnavailable: the provider cannot run here.
var ErrUnavailable = errors.New("not available here")

// ErrNoUndo: the session started before undo existed (version 3 or
// older): it would take an undo for a message.
var ErrNoUndo = errors.New("this session started before undo existed: start a new session to undo")

// ErrSessionFailed: the session stopped on an error (a store's, say:
// kairo#5) and takes no more messages. It goes on in a session forked
// from its last finished turn (ADR 0021).
var ErrSessionFailed = errors.New("this session stopped on an error and takes no more messages")

// ErrNoTurnToCancel: the session's last turn has ended (or none was sent).
var ErrNoTurnToCancel = errors.New("no turn is running")
