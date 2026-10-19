// Package acp is KairoCode as an ACP agent (Agent Client Protocol v1, ADR
// 0002): an editor starts `kairocode acp` and talks JSON-RPC over its stdin
// and stdout. The agent is a client of the daemon (ADR 0008): it maps the
// daemon's sessions, turns, events and approvals to ACP, and holds nothing
// that must outlive it.
package acp

import (
	"io"

	"github.com/r-hashi01/kairocode/internal/jsonrpc"
)

// The connection is internal/jsonrpc's.
type (
	message  = jsonrpc.Message
	rpcError = jsonrpc.Error
	handler  = jsonrpc.Handler
	conn     = jsonrpc.Conn
)

const (
	codeMethodNotFound = jsonrpc.CodeMethodNotFound
	codeInvalidParams  = jsonrpc.CodeInvalidParams
)

func newConn(w io.Writer) *conn { return jsonrpc.NewConn(w) }
