package workflows

import kairo "github.com/r-hashi01/kairo/sdk/go"

// RunMCPCall is one tool call that Claude Code made over MCP (ADR 0007). It
// is a workflow of its own so that its id, chosen by the MCP server from the
// call's content, makes a repeated call meet its earlier run.
func RunMCPCall(ctx *kairo.Context, in MCPCallInput) (ToolOutput, error) {
	return callTool(ctx, ActToolPrefix+in.Tool, in.Planned, ToolInput{Dir: in.Dir, Args: in.Args})
}
