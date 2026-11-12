package esl

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func parse(s string) (*frame, error) {
	return readFrame(bufio.NewReaderSize(strings.NewReader(s), readBufSize))
}

func TestReadFrame(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(
		"\nContent-Type: api/response\r\nContent-Length: 5\n\nab\ncdContent-Type: command/reply\nReply-Text: +OK\n\n"),
		readBufSize)
	f, err := readFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	if f.header["Content-Type"] != "api/response" || string(f.body) != "ab\ncd" {
		t.Errorf("first frame = %v %q", f.header, f.body)
	}
	f, err = readFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	if f.header["Reply-Text"] != "+OK" || f.body != nil {
		t.Errorf("second frame = %v %q", f.header, f.body)
	}
	if _, err := readFrame(r); !errors.Is(err, io.EOF) {
		t.Errorf("at end: err = %v, want EOF", err)
	}
}

func TestReadFrameRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"huge length":     "Content-Type: api/response\nContent-Length: 99999999999\n\n",
		"negative length": "Content-Length: -1\n\n",
		"junk length":     "Content-Length: 12abc\n\n",
		"long line":       "X-Long: " + strings.Repeat("a", readBufSize+10) + "\n\n",
		"no colon":        "garbage\n\n",
		"short body":      "Content-Length: 10\n\nabc",
	} {
		if _, err := parse(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := parse("X-Long: " + strings.Repeat("a", readBufSize+10) + "\n\n"); !errors.Is(err, errLineTooLong) {
		t.Errorf("long line err = %v", err)
	}
}

func TestDecodeEvent(t *testing.T) {
	h, body, err := decodeEvent([]byte(`{"Event-Name":"HEARTBEAT","Null":null,"Obj":{"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if h["Event-Name"] != "HEARTBEAT" || h["Null"] != "" || h["Obj"] != `{"a":1}` || body != "" {
		t.Errorf("headers = %v body = %q", h, body)
	}
	if _, _, err := decodeEvent([]byte(`[1,2]`)); err == nil {
		t.Error("non-object accepted")
	}
}
