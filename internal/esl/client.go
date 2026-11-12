package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	writeTimeout     = 10 * time.Second
	maxBackoff       = 30 * time.Second
)

// Client is an inbound ESL client. It keeps one authenticated connection to
// mod_event_socket open and reconnects when it drops.
type Client struct {
	addr     string
	password string
	backoff  time.Duration // first reconnect delay; tests shorten it

	ctx      context.Context
	cancel   context.CancelFunc
	logCh    chan struct{} // signals the loop that logLevel changed
	loopDone chan struct{}

	closeOnce sync.Once

	mu       sync.Mutex
	started  bool
	closed   bool
	state    State
	cur      *conn
	logLevel int
	subs     map[chan Message]struct{}
}

// NewClient returns a client for addr ("host:port"). It does not connect.
func NewClient(addr, password string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		addr:     addr,
		password: password,
		backoff:  time.Second,
		ctx:      ctx,
		cancel:   cancel,
		logCh:    make(chan struct{}, 1),
		loopDone: make(chan struct{}),
		state:    State{Since: time.Now()},
		logLevel: -1,
		subs:     make(map[chan Message]struct{}),
	}
}

// Start launches the background connect loop: dial, authenticate, send
// "event json HEARTBEAT", re-apply the log level, then read frames until the
// connection drops; reconnect with backoff (1s doubling to 30s, reset after a
// successful auth). Calling Start twice is a no-op.
func (c *Client) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.closed {
		return
	}
	c.started = true
	go c.run()
}

// Close stops the loop, closes the connection and closes every subscriber
// channel. Safe to call more than once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		started := c.started
		c.mu.Unlock()

		c.cancel()
		if started {
			<-c.loopDone
		}

		c.mu.Lock()
		for ch := range c.subs {
			delete(c.subs, ch)
			close(ch)
		}
		c.mu.Unlock()
	})
}

// State returns the current connection state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// API runs "api <command>" and returns the response body verbatim (including
// bodies that start with "-ERR"; the caller interprets them). Errors are
// transport-level only: ErrNotConnected, ErrInvalidCommand (command contains
// \n or \r), ctx errors, read/write errors. Commands are serialized; replies
// are matched in order. If ctx ends while a reply is pending, the connection
// is closed (reply ordering is lost) and the loop reconnects.
func (c *Client) API(ctx context.Context, command string) (string, error) {
	if strings.ContainsAny(command, "\r\n") {
		return "", ErrInvalidCommand
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	cn := c.cur
	c.mu.Unlock()
	if cn == nil {
		return "", ErrNotConnected
	}
	ch, err := cn.send(ctx, "api "+command)
	if err != nil {
		return "", err
	}
	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		select {
		case r := <-ch:
			return r.body, r.err
		default:
		}
		// The late reply would be matched to the next caller, so the
		// connection cannot be reused.
		cn.fail(fmt.Errorf("esl: api reply not received: %w", ctx.Err()))
		return "", ctx.Err()
	}
}

// SetLogLevel sets the ESL log level for this connection: -1 turns logging
// off ("nolog"), 0..7 sends "log <level>". Applied immediately when connected
// and re-applied after every reconnect. Not connected is not an error.
// Values below -1 count as -1 and values above 7 as 7.
func (c *Client) SetLogLevel(level int) {
	level = max(-1, min(level, 7))
	c.mu.Lock()
	c.logLevel = level
	c.mu.Unlock()
	// The loop goroutine does the write, so callers never wait on the socket
	// and the latest level always wins.
	select {
	case c.logCh <- struct{}{}:
	default:
	}
}

// Subscribe returns a channel of asynchronous messages and a cancel func.
// A subscriber that does not keep up loses messages (non-blocking send);
// the client never blocks on a subscriber. cancel closes the channel.
// Messages are shared between subscribers and must not be modified.
func (c *Client) Subscribe(buffer int) (<-chan Message, func()) {
	ch := make(chan Message, max(buffer, 0))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		close(ch)
		return ch, func() {}
	}
	c.subs[ch] = struct{}{}
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.subs[ch]; ok {
			delete(c.subs, ch)
			close(ch)
		}
	}
}

func (c *Client) publish(m Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publishLocked(m)
}

func (c *Client) publishLocked(m Message) {
	for ch := range c.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

// setState records a state change and announces it while holding the lock,
// so subscribers see state messages in the order they happened.
func (c *Client) setState(cn *conn, errText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = cn
	connected := cn != nil
	if c.state.Connected == connected && c.state.LastError == errText {
		return
	}
	if c.state.Connected != connected {
		c.state.Since = time.Now()
	}
	c.state.Connected = connected
	c.state.LastError = errText
	c.publishLocked(Message{
		Kind:    KindState,
		Headers: map[string]string{"connected": strconv.FormatBool(connected), "error": errText},
	})
}

func (c *Client) setDown(err error) {
	text := err.Error()
	if c.ctx.Err() != nil {
		text = "esl: client closed"
	}
	c.setState(nil, text)
}

func (c *Client) run() {
	defer close(c.loopDone)
	delay := c.backoff
	for {
		if c.session() {
			delay = c.backoff
		}
		t := time.NewTimer(delay)
		select {
		case <-c.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(delay*2, maxBackoff)
	}
}

// session runs one connection from dial to drop and reports whether it got
// past authentication.
func (c *Client) session() bool {
	d := net.Dialer{Timeout: dialTimeout}
	nc, err := d.DialContext(c.ctx, "tcp", c.addr)
	if err != nil {
		c.setDown(err)
		return false
	}
	stop := context.AfterFunc(c.ctx, func() { nc.Close() })
	defer stop()

	r := bufio.NewReaderSize(nc, readBufSize)
	if err := c.handshake(nc, r); err != nil {
		nc.Close()
		c.setDown(err)
		return false
	}

	cn := &conn{nc: nc, wsem: make(chan struct{}, 1), dead: make(chan struct{})}
	select {
	case <-c.logCh: // the level read below already covers it
	default:
	}
	c.mu.Lock()
	level := c.logLevel
	c.mu.Unlock()
	// Replies to setup commands are consumed by the reader and ignored.
	cmds := []string{"event json HEARTBEAT"}
	if level >= 0 {
		cmds = append(cmds, logCommand(level))
	}
	for _, cmd := range cmds {
		if _, err := cn.send(c.ctx, cmd); err != nil {
			cn.fail(err)
			c.setDown(cn.reason())
			return true
		}
	}

	c.setState(cn, "")
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(cn, r) }()
	for {
		select {
		case err := <-readErr:
			cn.fail(err)
			c.setDown(cn.reason())
			return true
		case <-c.logCh:
			c.mu.Lock()
			level := c.logLevel
			c.mu.Unlock()
			cn.send(c.ctx, logCommand(level))
		}
	}
}

func logCommand(level int) string {
	if level < 0 {
		return "nolog"
	}
	return "log " + strconv.Itoa(level)
}

func (c *Client) handshake(nc net.Conn, r *bufio.Reader) error {
	if strings.ContainsAny(c.password, "\r\n") {
		return ErrAuth
	}
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	f, err := readFrame(r)
	if err != nil {
		return fmt.Errorf("esl: read: %w", err)
	}
	switch ct := f.header["Content-Type"]; ct {
	case "auth/request":
	case "text/rude-rejection":
		return rejection(f)
	default:
		return fmt.Errorf("esl: unexpected %q before auth", ct)
	}
	if _, err := io.WriteString(nc, "auth "+c.password+"\n\n"); err != nil {
		return fmt.Errorf("esl: write: %w", err)
	}
	f, err = readFrame(r)
	if err != nil {
		return fmt.Errorf("esl: read: %w", err)
	}
	if ct := f.header["Content-Type"]; ct != "command/reply" {
		return fmt.Errorf("esl: unexpected %q after auth", ct)
	}
	if !strings.HasPrefix(f.header["Reply-Text"], "+OK") {
		return ErrAuth
	}
	return nc.SetDeadline(time.Time{})
}

func (c *Client) readLoop(cn *conn, r *bufio.Reader) error {
	for {
		f, err := readFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("esl: connection closed by server")
			}
			return fmt.Errorf("esl: read: %w", err)
		}
		switch f.header["Content-Type"] {
		case "api/response":
			cn.complete(result{body: string(f.body)})
		case "command/reply":
			cn.complete(result{body: f.header["Reply-Text"]})
		case "text/event-json":
			// A malformed event is dropped rather than killing the connection.
			if h, body, err := decodeEvent(f.body); err == nil {
				c.publish(Message{Kind: KindEvent, Headers: h, Body: body})
			}
		case "log/data":
			c.publish(Message{Kind: KindLog, Headers: f.header, Body: string(f.body)})
		case "text/disconnect-notice":
			return fmt.Errorf("esl: disconnected by server: %s", strings.TrimSpace(string(f.body)))
		case "text/rude-rejection":
			return rejection(f)
		}
	}
}

type result struct {
	body string
	err  error
}

// conn is one authenticated connection. Writers hold wsem while queueing a
// waiter and writing, so the waiter queue has the same order as the wire.
type conn struct {
	nc   net.Conn
	wsem chan struct{}
	dead chan struct{} // closed by fail

	mu      sync.Mutex
	pending []chan result
	err     error // why the connection ended
}

func (cn *conn) send(ctx context.Context, cmd string) (<-chan result, error) {
	select {
	case cn.wsem <- struct{}{}:
	case <-cn.dead:
		return nil, ErrNotConnected
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-cn.wsem }()

	ch := make(chan result, 1)
	cn.mu.Lock()
	if cn.err != nil {
		cn.mu.Unlock()
		return nil, ErrNotConnected
	}
	cn.pending = append(cn.pending, ch)
	cn.mu.Unlock()

	cn.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(cn.nc, cmd+"\n\n"); err != nil {
		err = fmt.Errorf("esl: write: %w", err)
		cn.fail(err)
		return nil, err
	}
	return ch, nil
}

func (cn *conn) complete(r result) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if len(cn.pending) == 0 {
		return
	}
	ch := cn.pending[0]
	cn.pending = cn.pending[1:]
	ch <- r
}

// fail closes the connection and fails every pending reply; only the first
// reason is kept.
func (cn *conn) fail(reason error) {
	cn.mu.Lock()
	if cn.err == nil {
		cn.err = reason
		close(cn.dead)
		for _, ch := range cn.pending {
			ch <- result{err: fmt.Errorf("%w: %v", ErrNotConnected, reason)}
		}
		cn.pending = nil
	}
	cn.mu.Unlock()
	cn.nc.Close()
}

func (cn *conn) reason() error {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.err
}
