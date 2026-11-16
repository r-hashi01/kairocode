package acp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/r-hashi01/kairocode/internal/api"
	"github.com/r-hashi01/kairocode/internal/tools"
)

// items lists the session's work items, and what they wait for (ADR
// 0043).
func (a *agent) items(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return nil, invalid(errors.New(`want {"sessionId": "<id>"}`))
	}
	items, err := a.b.Items(ctx, p.SessionID)
	if err != nil {
		return nil, invalid(err)
	}
	all, err := a.b.PendingAll(ctx, p.SessionID)
	if err != nil {
		return nil, invalid(err)
	}
	waiting := []api.Pending{}
	for _, x := range all {
		if x.Item != "" {
			waiting = append(waiting, x)
		}
	}
	if items == nil {
		items = []api.Item{}
	}
	return map[string]any{"items": items, "waiting": waiting}, nil
}

// line starts an item down the standard line the person named (/line).
func (a *agent) line(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		tools.ItemSpec
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.Line == "" || p.Request == "" {
		return nil, invalid(errors.New(`want {"sessionId", "line", "request"}`))
	}
	it, err := a.b.StartItem(ctx, p.SessionID, p.ItemSpec)
	if err != nil {
		return nil, invalid(err)
	}
	return it, nil
}

// dropItem drops one of the session's items.
func (a *agent) dropItem(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Item      string `json:"item"`
		Force     bool   `json:"force"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.Item == "" {
		return nil, invalid(errors.New(`want {"sessionId", "item", "force"}`))
	}
	if err := a.b.DropItem(ctx, p.SessionID, p.Item, p.Force); err != nil {
		return nil, invalid(err)
	}
	return map[string]any{}, nil
}

// keepLine keeps the line an item ran as a line of the project.
func (a *agent) keepLine(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Item      string `json:"item"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.Item == "" || p.Name == "" {
		return nil, invalid(errors.New(`want {"sessionId", "item", "name"}`))
	}
	path, err := a.b.KeepLine(ctx, p.SessionID, p.Item, p.Name)
	if err != nil {
		return nil, invalid(err)
	}
	return map[string]any{"path": path}, nil
}

// itemAnswer answers what an item waits for: an approval ({"key", "ok"})
// or a question ({"key", "option"}). The person answers, through the
// client: never the conversation's model.
func (a *agent) itemAnswer(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Key    string `json:"key"`
		OK     bool   `json:"ok"`
		Option string `json:"option"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Key == "" {
		return nil, invalid(errors.New(`want {"key", "ok"} or {"key", "option"}`))
	}
	var err error
	if p.Option != "" {
		err = a.b.Answer(ctx, p.Key, p.Option)
	} else {
		err = a.b.Approve(ctx, p.Key, p.OK, "")
	}
	if err != nil {
		return nil, invalid(err)
	}
	return map[string]any{}, nil
}

// Items are the session's work items and what they wait for (only
// KairoCode's agent has this; ADR 0043).
func (cl *Client) Items(ctx context.Context, session string) ([]api.Item, []api.Pending, error) {
	var out struct {
		Items   []api.Item    `json:"items"`
		Waiting []api.Pending `json:"waiting"`
	}
	err := cl.c.Call(ctx, ItemsRPC, map[string]any{"sessionId": session}, &out)
	return out.Items, out.Waiting, err
}

// Line starts an item down the standard line named.
func (cl *Client) Line(ctx context.Context, session, line, request string) (api.Item, error) {
	var out api.Item
	err := cl.c.Call(ctx, LineRPC, map[string]any{"sessionId": session, "line": line, "request": request}, &out)
	return out, err
}

// DropItem drops one of the session's items.
func (cl *Client) DropItem(ctx context.Context, session, item string, force bool) error {
	return cl.c.Call(ctx, DropItemRPC, map[string]any{"sessionId": session, "item": item, "force": force}, nil)
}

// KeepLine keeps the line an item ran as .kairocode/lines/<name>.json:
// the file's path.
func (cl *Client) KeepLine(ctx context.Context, session, item, name string) (string, error) {
	var out struct {
		Path string `json:"path"`
	}
	err := cl.c.Call(ctx, KeepLineRPC, map[string]any{"sessionId": session, "item": item, "name": name}, &out)
	return out.Path, err
}

// ItemAnswer answers what an item waits for: option for a question, else
// ok for an approval.
func (cl *Client) ItemAnswer(ctx context.Context, key string, ok bool, option string) error {
	return cl.c.Call(ctx, ItemAnswerRPC, map[string]any{"key": key, "ok": ok, "option": option}, nil)
}
