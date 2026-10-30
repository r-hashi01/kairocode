package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// A handler's Then: its answer is written before what After sends.
func TestThenAnswersFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := NewConn(outW)
	go c.Serve(ctx, inR, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return Then{Result: "loaded", After: func() { c.Notify("asked", nil) }}, nil
	})
	if _, err := inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"load"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(outR)
	var got []Message
	for len(got) < 2 {
		b, err := lines.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m Message
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	if string(got[0].ID) != "1" || string(got[0].Result) != `"loaded"` || got[1].Method != "asked" {
		t.Fatalf("got %+v", got)
	}
	inW.Close()
}
