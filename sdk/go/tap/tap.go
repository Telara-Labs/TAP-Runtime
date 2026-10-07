// Package tap provides the minimal JSON-line broker client for compiled Go
// primitives running as WASI Preview1 guests.
//
// It deliberately exposes only the declared file-read operation and the
// runner's final return frame. It performs no filesystem or environment access
// itself beyond the supplied protocol streams.
package tap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// ErrorCode identifies a broker failure without parsing its text.
type ErrorCode string

const (
	ErrProtocol  ErrorCode = "protocol"
	ErrRefused   ErrorCode = "refused"
	ErrViolation ErrorCode = "violation"
	ErrUnknown   ErrorCode = "unknown"
	ErrFailed    ErrorCode = "failed"
)

// Error is returned when a broker response is refused, untrusted, or malformed.
type Error struct {
	Code   ErrorCode
	Landed bool
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}

// Reply preserves the runner fields callers may need to interpret. Exit is
// the native command status; Status is the HTTP status from fetch.
type Reply struct {
	ID        string   `json:"id,omitempty"`
	Result    string   `json:"result,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Stdout    string   `json:"stdout"`
	Stderr    string   `json:"stderr"`
	Exit      int      `json:"exit"`
	Stdin     string   `json:"stdin"`
	Status    int      `json:"status,omitempty"`
	Refused   string   `json:"refused,omitempty"`
	Violation bool     `json:"violation,omitempty"`
	Landed    bool     `json:"landed,omitempty"`
	Unknown   bool     `json:"unknown,omitempty"`
	Gated     bool     `json:"gated,omitempty"`
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
}

type rawResponse struct {
	Reply
	Result json.RawMessage `json:"result"`
}

var allowedMethods = map[string]bool{
	"tools": true, "call": true, "exec": true,
	"read": true, "write": true, "fetch": true,
}

// Client sends one request at a time and matches each answer by request ID.
type Client struct {
	in   *bufio.Reader
	out  *bufio.Writer
	next uint64
}

// New creates a client using the WASI program's standard input and output.
func New() *Client { return NewWithIO(os.Stdin, os.Stdout) }

// NewWithIO creates a client over protocol streams. It is useful for tests and
// does not open files or inspect the process environment.
func NewWithIO(in io.Reader, out io.Writer) *Client {
	return &Client{in: bufio.NewReader(in), out: bufio.NewWriter(out)}
}

// Request sends one supported broker method with method-specific fields.
// Exit and Status remain caller-owned so exec/fetch callers can inspect them.
func (c *Client) Request(method string, fields map[string]any) (Reply, error) {
	if !allowedMethods[method] {
		return Reply{}, &Error{Code: ErrProtocol, Detail: "unsupported broker method: " + method}
	}
	if _, ok := fields["id"]; ok {
		return Reply{}, &Error{Code: ErrProtocol, Detail: "id is reserved for the SDK"}
	}
	if _, ok := fields["method"]; ok {
		return Reply{}, &Error{Code: ErrProtocol, Detail: "method is reserved for the SDK"}
	}
	c.next++
	id := "r" + strconv.FormatUint(c.next, 10)
	frame := make(map[string]any, len(fields)+2)
	frame["id"], frame["method"] = id, method
	for key, value := range fields {
		frame[key] = value
	}
	if err := c.send(frame); err != nil {
		return Reply{}, err
	}
	line, err := c.in.ReadBytes('\n')
	if err != nil {
		return Reply{}, &Error{Code: ErrProtocol, Detail: "read broker response: " + err.Error()}
	}
	var raw rawResponse
	if err := json.Unmarshal(line, &raw); err != nil {
		return Reply{}, &Error{Code: ErrProtocol, Detail: "decode broker response: " + err.Error()}
	}
	reply := raw.Reply
	if reply.ID != id {
		return reply, &Error{Code: ErrProtocol, Detail: fmt.Sprintf("response ID %q does not match request ID %q", reply.ID, id)}
	}
	if reply.Unknown {
		return reply, &Error{Code: ErrUnknown, Detail: reply.Refused}
	}
	if reply.Refused != "" {
		return reply, &Error{Code: ErrRefused, Detail: reply.Refused}
	}
	if reply.Violation {
		return reply, &Error{Code: ErrViolation, Landed: reply.Landed, Detail: reply.Stderr}
	}
	if len(raw.Result) > 0 {
		if err := json.Unmarshal(raw.Result, &reply.Result); err != nil {
			return reply, &Error{Code: ErrProtocol, Detail: "broker result is not a string"}
		}
	}
	return reply, nil
}

// Read requests the contents of one path declared with access: read.
func (c *Client) Read(path string) (string, error) {
	if path == "" {
		return "", &Error{Code: ErrProtocol, Detail: "read path must not be empty"}
	}
	reply, err := c.Request("read", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	if reply.Exit != 0 {
		return "", &Error{Code: ErrFailed, Detail: "read failed: " + reply.Stderr}
	}
	return reply.Result, nil
}

// Call invokes one declared connector tool alias and preserves its result.
func (c *Client) Call(alias string, arguments map[string]any) (Reply, error) {
	if alias == "" {
		return Reply{}, &Error{Code: ErrProtocol, Detail: "tool alias must not be empty"}
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	reply, err := c.Request("call", map[string]any{"alias": alias, "arguments": arguments})
	if err != nil {
		return reply, err
	}
	if reply.Exit != 0 {
		return reply, &Error{Code: ErrFailed, Detail: "call failed: " + reply.Stderr}
	}
	return reply, nil
}

// Return sends the final guest result to the runner. No further broker calls
// should be made after it succeeds.
func (c *Client) Return(stdout, stderr string, exit int) error {
	if exit < 0 {
		return errors.New("exit code must be non-negative")
	}
	return c.send(struct {
		Method string `json:"method"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Exit   int    `json:"exit"`
	}{Method: "return", Stdout: stdout, Stderr: stderr, Exit: exit})
}

func (c *Client) send(value any) error {
	if err := json.NewEncoder(c.out).Encode(value); err != nil {
		return fmt.Errorf("encode broker frame: %w", err)
	}
	if err := c.out.Flush(); err != nil {
		return fmt.Errorf("flush broker frame: %w", err)
	}
	return nil
}
