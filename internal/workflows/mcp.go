package workflows

import kairo "github.com/r-hashi01/kairo/sdk/go"

// RunMCPCall is one tool call that Claude Code made over MCP (ADR 0007). It
// is a workflow of its own so that its id, chosen by the MCP server from the
// call's content, makes a repeated call meet its earlier run.
func RunMCPCall(ctx *kairo.Context, in MCPCallInput) (ToolOutput, error) {
	ti := ToolInput{Dir: in.Dir, Args: in.Args}
	mcp := in.MCP
	run := HookRunInput{Dir: in.Dir, Session: in.Session, Tool: in.Tool, Call: &ti}
	if hasCommandHook(in.Hooks, HookToolBefore) {
		// Claude Code's PreToolUse, for a call any engine makes over MCP
		// (ADR 0018): a hook that blocks it stops it.
		_, blocked, err := commandHooks(ctx, in.Hooks, run, HookToolBefore)
		if err != nil {
			return ToolOutput{}, err
		}
		if blocked != "" {
			return kairo.Call[ToolOutput](ctx, ActToolDenied, DeniedInput{Reason: "a hook stopped this call: " + blocked})
		}
	}
	g, err := gateTool(ctx, in.Policy, in.Tool, in.Planned, ti, &mcp, nil)
	if err != nil {
		return ToolOutput{}, err
	}
	out, err := runGated(ctx, g, ti)
	if err != nil || !ran(g) || !hasCommandHook(in.Hooks, HookToolAfter) {
		return out, err
	}
	// Its PostToolUse: what a hook that does not pass says goes with the
	// call's result.
	run.Result = out.Message
	_, blocked, err := commandHooks(ctx, in.Hooks, run, HookToolAfter)
	if err != nil || blocked == "" {
		return out, err
	}
	return kairo.Call[ToolOutput](ctx, ActHookNote, HookNoteInput{Result: out, Note: blocked})
}
