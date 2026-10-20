package workflows

import kairo "github.com/r-hashi01/kairo/sdk/go"

// RunMCPCall is one tool call that Claude Code made over MCP (ADR 0007). It
// is a workflow of its own so that its id, chosen by the MCP server from the
// call's content, makes a repeated call meet its earlier run.
func RunMCPCall(ctx *kairo.Context, in MCPCallInput) (ToolOutput, error) {
	ti := ToolInput{Dir: in.Dir, Args: in.Args}
	mcp := in.MCP
	g, err := gateTool(ctx, in.Policy, in.Tool, in.Planned, ti, &mcp, nil)
	if err != nil {
		return ToolOutput{}, err
	}
	return runGated(ctx, g, ti)
}
