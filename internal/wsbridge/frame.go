// Package wsbridge implements the chat side of ADR 13: the HTTP endpoints
// ../../../ws2api calls, and the pusher that sends frames back through it.
//
// The API holds no WebSocket connections. A connection's whole lifecycle
// arrives here as three ordinary requests, and a server-initiated push is an
// HTTP post to the proxy. That is what keeps the API tier stateless and
// horizontally scalable. See ADR 18 for the frame format and the reasoning.
package wsbridge

import (
	"encoding/json"
	"fmt"
)

// Frame types travelling from the client to the server.
const (
	TypeHello    = "hello"
	TypeMessage  = "message"
	TypeTyping   = "typing"
	TypeRead     = "read"
	TypePresence = "presence"
	TypePing     = "ping"
)

// Frame types travelling from the server to the client. Message, typing and
// presence appear in both directions with different payloads: one is a request,
// the other is the event it produces for everyone else.
const (
	TypeReady = "ready"
	TypeAck   = "ack"
	TypeError = "error"
	TypePong  = "pong"
)

// Frame is one text frame. Every field beyond `type` is optional, because a
// frame carries only what its type needs, and unknown fields are ignored so a
// newer client can add one without breaking an older server.
type Frame struct {
	Type string `json:"type"`
	// ID correlates an acknowledgement with the frame that caused it. The
	// transport gives no correlation of its own: a push and a response arrive
	// on the same socket and look alike (ADR 13).
	ID string `json:"id,omitempty"`

	ChatID string `json:"chatId,omitempty"`
	Body   string `json:"body,omitempty"`
	On     *bool  `json:"on,omitempty"`
	Status string `json:"status,omitempty"`

	// Fields the server fills in.
	UserID    int64           `json:"userId,omitempty"`
	UserName  string          `json:"userName,omitempty"`
	Email     string          `json:"email,omitempty"`
	MessageID string          `json:"messageId,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Decode parses a client frame. A frame that is not an object with a known type
// is refused here rather than reaching a handler.
func Decode(body []byte) (*Frame, error) {
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("frame is not JSON: %w", err)
	}
	switch f.Type {
	case TypeHello, TypeMessage, TypeTyping, TypeRead, TypePresence, TypePing:
		return &f, nil
	case "":
		return nil, fmt.Errorf("frame has no type")
	default:
		return nil, fmt.Errorf("unknown frame type %q", f.Type)
	}
}

// Encode renders a frame for the wire.
func Encode(f *Frame) []byte {
	body, err := json.Marshal(f)
	if err != nil {
		// A Frame cannot fail to marshal; if it somehow does, an error frame
		// is still better than a closed socket.
		return []byte(`{"type":"error","error":"could not encode the response"}`)
	}
	return body
}

// errorFrame is the answer to anything the server could not do. The correlation
// id is echoed so a client waiting on a specific frame stops waiting.
func errorFrame(id, message string) []byte {
	return Encode(&Frame{Type: TypeError, ID: id, Error: message})
}

// on reports the value of an optional boolean, defaulting to true. A typing
// frame that omits it means typing started, which is the common case.
func on(v *bool) bool { return v == nil || *v }
