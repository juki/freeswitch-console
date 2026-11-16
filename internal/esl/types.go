// Package esl implements a client for the FreeSWITCH Event Socket Layer in
// inbound mode: the client connects to mod_event_socket, authenticates, runs
// API commands and receives events and log lines.
package esl

import (
	"errors"
	"time"
)

// Message kinds delivered to subscribers.
const (
	KindEvent = "event" // a FreeSWITCH event
	KindLog   = "log"   // a log line, enabled with SetLogLevel
	KindState = "state" // the connection state changed
)

// Message is an asynchronous frame pushed to subscribers.
//
//   - KindEvent: Headers holds the event's own headers (Event-Name,
//     FreeSWITCH-Hostname, ...); Body is the event body, if any.
//   - KindLog: Headers holds the frame headers (Log-Level, Log-File, Log-Func,
//     Log-Line, Text-Channel, User-Data); Body is the log text.
//   - KindState: Headers holds "connected" ("true" or "false") and "error".
type Message struct {
	Kind    string
	Headers map[string]string
	Body    string
}

// State describes the connection of a Client.
type State struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`     // time of the last state change
	LastError string    `json:"lastError"` // last connect or read error; empty while connected
}

var (
	// ErrNotConnected is returned by API and Background while the client
	// has no authenticated connection.
	ErrNotConnected = errors.New("esl: not connected")

	// ErrInvalidCommand is returned for a command that contains a line break,
	// which would let the caller smuggle a second ESL command.
	ErrInvalidCommand = errors.New("esl: command contains a line break")

	// ErrAuth is reported in State.LastError when the password is rejected.
	ErrAuth = errors.New("esl: authentication failed")
)
