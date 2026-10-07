package tap

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestReadUsesRequestIDAndReturnsString(t *testing.T) {
	var out bytes.Buffer
	client := NewWithIO(strings.NewReader(`{"id":"r1","result":"tasks"}`+"\n"), &out)
	got, err := client.Read("tasks.json")
	if err != nil || got != "tasks" {
		t.Fatalf("Read() = %q, %v; want tasks, nil", got, err)
	}
	if out.String() != "{\"id\":\"r1\",\"method\":\"read\",\"path\":\"tasks.json\"}\n" {
		t.Fatalf("request = %q", out.String())
	}
}

func TestReadRefusesMismatchedOrMalformedResponse(t *testing.T) {
	for name, line := range map[string]string{
		"mismatched ID": `{"id":"other","result":"wrong"}` + "\n",
		"invalid JSON":  "not-json\n",
	} {
		t.Run(name, func(t *testing.T) {
			client := NewWithIO(strings.NewReader(line), &bytes.Buffer{})
			_, err := client.Read("tasks.json")
			var tapErr *Error
			if !errors.As(err, &tapErr) || tapErr.Code != ErrProtocol {
				t.Fatalf("Read() error = %v; want protocol error", err)
			}
		})
	}
}

func TestRequestPropagatesRefusedViolationAndUnknown(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		code       ErrorCode
		landed     bool
		wantDetail string
	}{
		{"refused", `"refused":"path was not declared"`, ErrRefused, false, "path was not declared"},
		{"violation", `"violation":true,"landed":true,"stderr":"result mismatch"`, ErrViolation, true, "result mismatch"},
		{"unknown", `"unknown":true,"refused":"the outcome of this write is unknown: an earlier run stopped while it was in progress"`, ErrUnknown, false, "the outcome of this write is unknown: an earlier run stopped while it was in progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewWithIO(strings.NewReader(`{"id":"r1",`+tc.body+"}\n"), &bytes.Buffer{})
			method, fields := "call", map[string]any{"alias": "lookup"}
			if tc.name == "unknown" {
				method, fields = "write", map[string]any{"path": "result.txt", "content": "x"}
			}
			reply, err := client.Request(method, fields)
			var tapErr *Error
			if !errors.As(err, &tapErr) || tapErr.Code != tc.code || tapErr.Landed != tc.landed || tapErr.Detail != tc.wantDetail {
				t.Fatalf("Request() = %#v, %v; want code %q landed %v", reply, err, tc.code, tc.landed)
			}
			if tc.name == "refused" && reply.Refused == "" {
				t.Fatal("Reply did not preserve refused status")
			}
			if tc.name == "violation" && (!reply.Violation || !reply.Landed) {
				t.Fatal("Reply did not preserve violation/landed status")
			}
			if tc.name == "unknown" && (!reply.Unknown || reply.Refused != tc.wantDetail) {
				t.Fatal("Reply did not preserve unknown status and original message")
			}
		})
	}
}

func TestCallEncodesConnectorAliasAndArguments(t *testing.T) {
	var out bytes.Buffer
	client := NewWithIO(strings.NewReader(`{"id":"r1","result":"{\"count\":3}"}`+"\n"), &out)
	reply, err := client.Call("search", map[string]any{"query": "open tasks"})
	if err != nil || reply.Result != `{"count":3}` {
		t.Fatalf("Call() = %#v, %v", reply, err)
	}
	if out.String() != "{\"alias\":\"search\",\"arguments\":{\"query\":\"open tasks\"},\"id\":\"r1\",\"method\":\"call\"}\n" {
		t.Fatalf("connector request = %q", out.String())
	}
}

func TestCallTurnsNonzeroExitIntoFailedError(t *testing.T) {
	client := NewWithIO(strings.NewReader(`{"id":"r1","exit":1,"stderr":"connector failed"}`+"\n"), &bytes.Buffer{})
	reply, err := client.Call("search", map[string]any{"query": "open tasks"})
	var tapErr *Error
	if !errors.As(err, &tapErr) || tapErr.Code != ErrFailed || tapErr.Detail != "call failed: connector failed" {
		t.Fatalf("Call() = %#v, %v; want failed error", reply, err)
	}
	if reply.Exit != 1 || reply.Stderr != "connector failed" {
		t.Fatalf("Call() reply lost failure fields: %#v", reply)
	}
}

func TestRequestLeavesCommandAndHTTPStatusesForCaller(t *testing.T) {
	client := NewWithIO(strings.NewReader(`{"id":"r1","exit":126,"status":429,"stdout":"blocked"}`+"\n"), &bytes.Buffer{})
	reply, err := client.Request("exec", map[string]any{"command": "example"})
	if err != nil || reply.Exit != 126 || reply.Status != 429 || reply.Stdout != "blocked" {
		t.Fatalf("Request() = %#v, %v", reply, err)
	}
}

func TestRequestLeavesFailedWriteExitForCaller(t *testing.T) {
	client := NewWithIO(strings.NewReader(`{"id":"r1","exit":1,"stderr":"write failed"}`+"\n"), &bytes.Buffer{})
	reply, err := client.Request("write", map[string]any{"path": "result.txt", "content": "x"})
	if err != nil || reply.Exit != 1 || reply.Stderr != "write failed" {
		t.Fatalf("Request(write) = %#v, %v; want caller-owned failure fields", reply, err)
	}
}

func TestReturnEmitsFinalFrame(t *testing.T) {
	var out bytes.Buffer
	client := NewWithIO(strings.NewReader(""), &out)
	if err := client.Return(`{"count":2}`, "", 0); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"method\":\"return\",\"stdout\":\"{\\\"count\\\":2}\",\"stderr\":\"\",\"exit\":0}\n" {
		t.Fatalf("return frame = %q", out.String())
	}
}
