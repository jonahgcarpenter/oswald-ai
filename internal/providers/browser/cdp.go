package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// cdpRequest is one JSON-RPC command.
type cdpRequest struct {
	ID     int64       `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

// cdpResponse is one JSON-RPC reply.
type cdpResponse struct {
	ID     *int64          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// cdpConn is a single-flight-multiplexed DevTools websocket with a background
// read loop. Calls carry ctx deadlines; upstream cancellation interrupts wait.
type cdpConn struct {
	ws      *websocket.Conn
	next    atomic.Int64
	pending sync.Map // int64 -> chan cdpResponse
	onEvent func(method string, params json.RawMessage)
	writeMu sync.Mutex
	closed  atomic.Bool
}

// dialCDP opens the debugger websocket and starts the read loop.
func dialCDP(ctx context.Context, wsURL string, onEvent func(string, json.RawMessage)) (*cdpConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial browser debugger: %w", err)
	}
	c := &cdpConn{ws: ws, onEvent: onEvent}
	c.next.Store(1)
	go c.readLoop()
	return c, nil
}

func (c *cdpConn) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.failAll(err)
			return
		}
		var msg cdpResponse
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != nil {
			if ch, ok := c.pending.LoadAndDelete(*msg.ID); ok {
				select {
				case ch.(chan cdpResponse) <- msg:
				default:
				}
			}
			continue
		}
		if msg.Method != "" && c.onEvent != nil {
			c.onEvent(msg.Method, msg.Params)
		}
	}
}

func (c *cdpConn) failAll(err error) {
	c.pending.Range(func(key, value interface{}) bool {
		select {
		case value.(chan cdpResponse) <- cdpResponse{Error: &struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		}{Message: err.Error()}}:
		default:
		}
		c.pending.Delete(key)
		return true
	})
}

// Call sends one DevTools command and waits for its reply.
func (c *cdpConn) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := c.next.Add(1)
	ch := make(chan cdpResponse, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)
	c.writeMu.Lock()
	err := c.ws.WriteJSON(cdpRequest{ID: id, Method: method, Params: params})
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("browser command %s failed: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("browser command %s failed: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// Close terminates the debugger connection.
func (c *cdpConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.ws.Close()
}
