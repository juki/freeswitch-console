package esl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	readBufSize   = 64 << 10 // also the longest accepted header line
	maxHeaders    = 1024
	maxBodyLength = 64 << 20
)

var errLineTooLong = errors.New("header line too long")

// frame is one ESL message: headers, a blank line, then Content-Length bytes.
type frame struct {
	header map[string]string
	body   []byte
}

func readFrame(r *bufio.Reader) (*frame, error) {
	h := make(map[string]string)
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, errLineTooLong
			}
			return nil, err
		}
		s := strings.TrimRight(string(line), "\r\n")
		if s == "" {
			if len(h) == 0 {
				continue // stray blank line between frames
			}
			break
		}
		if len(h) >= maxHeaders {
			return nil, errors.New("too many header lines")
		}
		name, value, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header line %q", truncate(s, 80))
		}
		h[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	f := &frame{header: h}
	if v, ok := h["Content-Length"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxBodyLength {
			return nil, fmt.Errorf("bad Content-Length %q", truncate(v, 32))
		}
		f.body = make([]byte, n)
		if _, err := io.ReadFull(r, f.body); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// decodeEvent turns a text/event-json body into headers and the event body.
// Repeated FreeSWITCH headers arrive as JSON arrays; they and any other
// non-string values are kept as their JSON text.
func decodeEvent(body []byte) (map[string]string, string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", err
	}
	h := make(map[string]string, len(raw))
	var evBody string
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			s = string(v)
		}
		if k == "_body" {
			evBody = s
			continue
		}
		h[k] = s
	}
	return h, evBody, nil
}

func rejection(f *frame) error {
	if msg := strings.TrimSpace(string(f.body)); msg != "" {
		return errors.New(msg)
	}
	return errors.New("esl: rejected by server")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
