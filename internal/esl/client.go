package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	writeTimeout     = 10 * time.Second
	maxBackoff       = 30 * time.Second
)

var errNoAnswer = errors.New("esl: no answer from FreeSWITCH")

// Client is an inbound ESL client. It keeps one authenticated connection to
// mod_event_socket open and reconnects when it drops.
type Client struct {
	addr     string
	password string
	events   []string      // event names subscribed after authentication
	backoff  time.Duration // first reconnect delay; tests shorten it

	// A connection that has received nothing for probeAfter is sent
	// "api uptime" and dropped if no reply arrives within probeTimeout.
	// Without it a peer that vanishes silently is only noticed by TCP
	// keepalive. Tests shorten both.
	probeAfter   time.Duration
	probeTimeout time.Duration

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

// NewClient returns a client for addr ("host:port") that subscribes to the
// named events ("HEARTBEAT", ...) on every connection; with none it receives
// no events. It does not connect.
func NewClient(addr, password string, events ...string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		addr:         addr,
		password:     password,
		events:       events,
		backoff:      time.Second,
		probeAfter:   30 * time.Second,
		probeTimeout: 10 * time.Second,
		ctx:          ctx,
		cancel:       cancel,
		logCh:        make(chan struct{}, 1),
		loopDone:     make(chan struct{}),
		state:        State{Since: time.Now()},
		logLevel:     -1,
		subs:         make(map[chan Message]struct{}),
	}
}

// Start launches the background connect loop: dial, authenticate, send
// "event json <events>", re-apply the log level, then read frames until the
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
	ch, err := cn.send(ctx, "api "+command, sendCall)
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

	cn := &conn{nc: nc, start: time.Now(), wsem: make(chan struct{}, 1), dead: make(chan struct{})}
	select {
	case <-c.logCh: // the level read below already covers it
	default:
	}
	c.mu.Lock()
	level := c.logLevel
	c.mu.Unlock()
	// Replies to setup commands are consumed by the reader and ignored.
	var cmds []string
	if len(c.events) > 0 {
		cmds = append(cmds, "event json "+strings.Join(c.events, " "))
	}
	if level >= 0 {
		cmds = append(cmds, logCommand(level))
	}
	for _, cmd := range cmds {
		if _, err := cn.send(c.ctx, cmd, sendIgnore); err != nil {
			cn.fail(err)
			c.setDown(cn.reason())
			return true
		}
	}

	c.setState(cn, "")
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(cn, r) }()
	idle := time.NewTimer(c.probeAfter)
	defer idle.Stop()
	var probe <-chan result // reply to the outstanding probe, if any
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
			cn.send(c.ctx, logCommand(level), sendIgnore)
		case <-idle.C:
			if probe != nil && len(probe) == 0 {
				// Handled like an abandoned API reply: the closed socket
				// ends readLoop and the case above reports this reason.
				cn.fail(errNoAnswer)
				continue
			}
			probe = nil
			if left := c.probeAfter - cn.idle(); left > 0 {
				idle.Reset(left)
				continue
			}
			ch, err := cn.send(c.ctx, "api uptime", sendProbe)
			switch {
			case err != nil:
				// The socket is closed, so readErr follows.
			case ch == nil:
				// An API call is pending and its own deadline covers
				// liveness; FreeSWITCH would answer a probe only after it.
				idle.Reset(c.probeAfter)
			default:
				probe = ch
				idle.Reset(c.probeTimeout)
			}
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
		cn.lastRx.Store(int64(time.Since(cn.start)))
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
	nc     net.Conn
	start  time.Time
	lastRx atomic.Int64 // when the last frame arrived, as time since start
	wsem   chan struct{}
	dead   chan struct{} // closed by fail

	mu      sync.Mutex
	pending []chan result // nil entries hold the place of ignored replies
	err     error         // why the connection ended
}

type sendMode int

const (
	sendCall   sendMode = iota // an API call waits for the reply
	sendIgnore                 // setup and log commands: the reply is dropped
	sendProbe                  // like sendCall, but sends nothing while an API reply is pending
)

// idle returns how long ago the last frame arrived.
func (cn *conn) idle() time.Duration {
	return time.Since(cn.start) - time.Duration(cn.lastRx.Load())
}

// send writes cmd and queues its place in the reply order. The channel is nil
// for sendIgnore, and for sendProbe when nothing was sent. The probe check runs
// under wsem, so no API call can be written between the check and the probe.
func (cn *conn) send(ctx context.Context, cmd string, mode sendMode) (<-chan result, error) {
	select {
	case cn.wsem <- struct{}{}:
	case <-cn.dead:
		return nil, ErrNotConnected
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-cn.wsem }()

	var ch chan result
	if mode != sendIgnore {
		ch = make(chan result, 1)
	}
	cn.mu.Lock()
	if cn.err != nil {
		cn.mu.Unlock()
		return nil, ErrNotConnected
	}
	// Only API replies have non-nil channels here: the session loop sends
	// at most one probe and only after its reply has been delivered.
	if mode == sendProbe && slices.ContainsFunc(cn.pending, func(p chan result) bool { return p != nil }) {
		cn.mu.Unlock()
		return nil, nil
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
	if ch != nil {
		ch <- r
	}
}

// fail closes the connection and fails every pending reply; only the first
// reason is kept.
func (cn *conn) fail(reason error) {
	cn.mu.Lock()
	if cn.err == nil {
		cn.err = reason
		close(cn.dead)
		for _, ch := range cn.pending {
			if ch != nil {
				ch <- result{err: fmt.Errorf("%w: %v", ErrNotConnected, reason)}
			}
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
