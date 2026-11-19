package ipc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeValidate(t *testing.T) {
	ok := &Request{Version: ProtocolVersion, ID: "1", Method: MethodPing}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	cases := map[string]*Request{
		"version":        {Version: 99, ID: "1", Method: MethodPing},
		"missing id":     {Version: ProtocolVersion, Method: MethodPing},
		"missing method": {Version: ProtocolVersion, ID: "1"},
		"long method":    {Version: ProtocolVersion, ID: "1", Method: strings.Repeat("x", 100)},
	}
	for name, req := range cases {
		if err := req.Validate(); err == nil {
			t.Fatalf("%s: invalid request accepted", name)
		}
	}
}

func TestResponseValidate(t *testing.T) {
	if err := (&Response{Version: ProtocolVersion, OK: false}).Validate(); err == nil {
		t.Fatal("failed response without error accepted")
	}
	if err := (&Response{Version: ProtocolVersion, OK: true, Error: NewError(CodeInternal, "x")}).Validate(); err == nil {
		t.Fatal("successful response with error accepted")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := &Request{Version: ProtocolVersion, ID: "abc", Method: MethodStatus}
	req.Params = json.RawMessage(`{"x":1}`)
	if err := WriteFrame(&buf, req); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatal("frame must be newline terminated")
	}
	var got Request
	if err := ReadFrame(bufio.NewReader(&buf), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != req.ID || got.Method != req.Method || string(got.Params) != `{"x":1}` {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	huge := make([]byte, MaxFrameBytes+10)
	for i := range huge {
		huge[i] = 'a'
	}
	huge[0] = '"'
	huge[len(huge)-1] = '"'
	if err := WriteFrame(&buf, json.RawMessage(huge)); err == nil {
		t.Fatal("oversize frame written")
	}

	// Reading an oversize frame must fail rather than grow a buffer.
	reader := bufio.NewReader(bytes.NewReader(append(huge, '\n')))
	var v any
	if err := ReadFrame(reader, &v); err == nil {
		t.Fatal("oversize frame read")
	}
}

func TestFrameEOFWithoutNewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(`{"version":1,"id":"1","method":"ping"}`))
	var req Request
	if err := ReadFrame(r, &req); err != nil {
		t.Fatalf("EOF-terminated frame should parse: %v", err)
	}
	if req.Method != MethodPing {
		t.Fatalf("method = %q", req.Method)
	}
}

func TestFrameRejectsInvalidJSON(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{not json}\n"))
	var req Request
	if err := ReadFrame(r, &req); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

// roundTrip runs one request through a FrameConn over a net.Pipe.
func TestClientCallRoundTrip(t *testing.T) {
	server, clientConn := net.Pipe()
	defer server.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		fc := NewFrameConn(server, 2*time.Second)
		var req Request
		if err := fc.Read(&req); err != nil {
			return
		}
		resp := &Response{Version: ProtocolVersion, ID: req.ID, OK: true}
		resp.Result = json.RawMessage(`{"pid":42}`)
		_ = fc.Write(resp)
	}()

	c := NewClient(clientConn, 2*time.Second)
	defer c.Close()
	var out PingResult
	if err := c.Call(&Request{Version: ProtocolVersion, ID: "req-1", Method: MethodPing}, &out); err != nil {
		t.Fatal(err)
	}
	if out.PID != 42 {
		t.Fatalf("pid = %d", out.PID)
	}
	<-serverDone
}

func TestClientCallPropagatesDaemonError(t *testing.T) {
	server, clientConn := net.Pipe()
	defer server.Close()

	go func() {
		fc := NewFrameConn(server, 2*time.Second)
		var req Request
		if err := fc.Read(&req); err != nil {
			return
		}
		_ = fc.Write(&Response{
			Version: ProtocolVersion, ID: req.ID, OK: false,
			Error: NewError(CodeNotInRoom, "not in a room"),
		})
	}()

	c := NewClient(clientConn, 2*time.Second)
	defer c.Close()
	err := c.Call(&Request{Version: ProtocolVersion, ID: "req-2", Method: MethodPeersList}, nil)
	var ipcErr *Error
	if !errors.As(err, &ipcErr) {
		t.Fatalf("expected structured error, got %v", err)
	}
	if ipcErr.Code != CodeNotInRoom {
		t.Fatalf("code = %q", ipcErr.Code)
	}
}

func TestClientRejectsMismatchedResponseID(t *testing.T) {
	server, clientConn := net.Pipe()
	defer server.Close()

	go func() {
		fc := NewFrameConn(server, 2*time.Second)
		var req Request
		if err := fc.Read(&req); err != nil {
			return
		}
		_ = fc.Write(&Response{Version: ProtocolVersion, ID: "someone-else", OK: true})
	}()

	c := NewClient(clientConn, 2*time.Second)
	defer c.Close()
	if err := c.Call(&Request{Version: ProtocolVersion, ID: "req-3", Method: MethodPing}, nil); err == nil {
		t.Fatal("mismatched response id accepted")
	}
}
