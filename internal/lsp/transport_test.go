package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func frame(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeFrame(&buf, []byte(payload)); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	return buf.Bytes()
}

func TestReadFrame_CompleteMessage(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	r := bufio.NewReader(bytes.NewReader(frame(t, msg)))

	got, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("got %q, want %q", got, msg)
	}
}

// oneByteReader forces every downstream Read to see at most one byte at a
// time, simulating a message that arrives in many small fragments across
// several underlying reads.
type oneByteReader struct {
	r io.Reader
}

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestReadFrame_FragmentedAcrossManyReads(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":2,"method":"textDocument/definition","params":{"line":10}}`
	raw := frame(t, msg)
	r := bufio.NewReader(&oneByteReader{r: bytes.NewReader(raw)})

	got, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("got %q, want %q", got, msg)
	}
}

func TestReadFrame_TwoMessagesConcatenatedInOneRead(t *testing.T) {
	msg1 := `{"jsonrpc":"2.0","id":1,"result":{}}`
	msg2 := `{"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":"file:///a.go"}}`

	var buf bytes.Buffer
	buf.Write(frame(t, msg1))
	buf.Write(frame(t, msg2))

	r := bufio.NewReader(&buf)

	got1, err := readFrame(r)
	if err != nil {
		t.Fatalf("first readFrame: %v", err)
	}
	if string(got1) != msg1 {
		t.Fatalf("first message: got %q, want %q", got1, msg1)
	}

	got2, err := readFrame(r)
	if err != nil {
		t.Fatalf("second readFrame: %v", err)
	}
	if string(got2) != msg2 {
		t.Fatalf("second message: got %q, want %q", got2, msg2)
	}
}

func TestReadFrame_MissingContentLength(t *testing.T) {
	raw := "Content-Type: application/json\r\n\r\n{}"
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	if _, err := readFrame(r); err == nil {
		t.Fatal("expected an error for a message with no Content-Length header")
	}
}

func TestWriteFrame_RoundTripsThroughJSON(t *testing.T) {
	type payload struct {
		Foo string `json:"foo"`
	}
	data, err := json.Marshal(payload{Foo: "bar"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeFrame(&buf, data); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	r := bufio.NewReader(&buf)
	got, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	var out payload
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Foo != "bar" {
		t.Fatalf("got %+v", out)
	}
}
