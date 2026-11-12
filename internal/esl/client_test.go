package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testPassword = "ClueCon"

// fakeServer speaks enough of mod_event_socket's inbound protocol for tests.
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	password string
	silent   bool   // accept but never send auth/request
	reject   string // answer with text/rude-rejection carrying this body

	cmds         chan string    // every command received after auth
	conns        chan *fakeConn // every authenticated connection
	authAttempts atomic.Int32
	accepted     atomic.Int32

	mu   sync.Mutex
	open map[net.Conn]struct{}
	wg   sync.WaitGroup
}

type fakeConn struct {
	nc  net.Conn
	wmu sync.Mutex
}

func (fc *fakeConn) write(s string) {
	fc.wmu.Lock()
	defer fc.wmu.Unlock()
	fc.nc.Write([]byte(s))
}

func newFakeServer(t *testing.T, configure func(*fakeServer)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{
		t:        t,
		ln:       ln,
		password: testPassword,
		cmds:     make(chan string, 10000),
		conns:    make(chan *fakeConn, 100),
		open:     make(map[net.Conn]struct{}),
	}
	if configure != nil {
		configure(s)
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.close)
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		s.mu.Lock()
		s.open[nc] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.open, nc)
				s.mu.Unlock()
				nc.Close()
			}()
			s.serve(nc)
		}()
	}
}

// dropAll closes every open connection, as if FreeSWITCH restarted.
func (s *fakeServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for nc := range s.open {
		nc.Close()
	}
}

func (s *fakeServer) close() {
	s.ln.Close()
	s.dropAll()
	s.wg.Wait()
}

func readCmd(r *bufio.Reader) (string, error) {
	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(lines) == 0 {
				continue
			}
			return strings.Join(lines, "\n"), nil
		}
		lines = append(lines, line)
	}
}

func reply(text string) string {
	return "Content-Type: command/reply\nReply-Text: " + text + "\n\n"
}

func apiResponse(body string) string {
	return fmt.Sprintf("Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
}

func eventFrame(json string) string {
	return fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-json\n\n%s", len(json), json)
}

func logFrame(level int, text string) string {
	return fmt.Sprintf("Content-Type: log/data\nContent-Length: %d\nLog-Level: %d\nText-Channel: 3\n"+
		"Log-File: switch_core.c\nLog-Func: test_func\nLog-Line: 42\nUser-Data: \n\n%s", len(text), level, text)
}

func (s *fakeServer) serve(nc net.Conn) {
	fc := &fakeConn{nc: nc}
	if s.silent {
		io.Copy(io.Discard, nc)
		return
	}
	if s.reject != "" {
		fc.write(fmt.Sprintf("Content-Type: text/rude-rejection\nContent-Length: %d\n\n%s", len(s.reject), s.reject))
		return
	}
	fc.write("Content-Type: auth/request\n\n")
	r := bufio.NewReader(nc)
	cmd, err := readCmd(r)
	if err != nil {
		return
	}
	s.authAttempts.Add(1)
	if cmd != "auth "+s.password {
		fc.write(reply("-ERR invalid"))
		return
	}
	fc.write(reply("+OK accepted"))
	s.conns <- fc
	for {
		cmd, err := readCmd(r)
		if err != nil {
			return
		}
		s.cmds <- cmd
		s.handle(fc, cmd)
	}
}

func (s *fakeServer) handle(fc *fakeConn, cmd string) {
	switch {
	case strings.HasPrefix(cmd, "api echo "):
		fc.write(apiResponse(strings.TrimPrefix(cmd, "api echo ")))
	case cmd == "api multi":
		fc.write(apiResponse("line one\nline two\n\nline four\n"))
	case cmd == "api fail":
		fc.write(apiResponse("-ERR fail Command not found!\n"))
	case cmd == "api sleep":
		// never answered
	case cmd == "api event-first":
		fc.write(eventFrame(`{"Event-Name":"CUSTOM","_body":"not a reply"}`))
		fc.write(apiResponse("after event"))
	case cmd == "event json HEARTBEAT":
		fc.write(reply("+OK event listener enabled json"))
	case strings.HasPrefix(cmd, "log "):
		n := strings.TrimPrefix(cmd, "log ")
		fc.write(reply("+OK log level " + n + " [" + n + "]"))
	case cmd == "nolog":
		fc.write(reply("+OK no longer logging"))
	default:
		fc.write(reply("-ERR command not found"))
	}
}

// expectCmds reads commands until all of want have arrived in this order
// (other commands may be interleaved).
func (s *fakeServer) expectCmds(want ...string) {
	s.t.Helper()
	timeout := time.After(5 * time.Second)
	for len(want) > 0 {
		select {
		case got := <-s.cmds:
			if got == want[0] {
				want = want[1:]
			}
		case <-timeout:
			s.t.Fatalf("timed out waiting for command %q", want[0])
		}
	}
}

func (s *fakeServer) nextConn() *fakeConn {
	s.t.Helper()
	select {
	case fc := <-s.conns:
		return fc
	case <-time.After(5 * time.Second):
		s.t.Fatal("timed out waiting for an authenticated connection")
		return nil
	}
}

func newTestClient(t *testing.T, s *fakeServer) *Client {
	t.Helper()
	c := NewClient(s.addr(), testPassword)
	c.backoff = 10 * time.Millisecond
	t.Cleanup(c.Close)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitMsg(t *testing.T, ch <-chan Message, what string, match func(Message) bool) Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed while waiting for %s", what)
			}
			if match(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func isState(connected bool) func(Message) bool {
	return func(m Message) bool {
		return m.Kind == KindState && m.Headers["connected"] == strconv.FormatBool(connected)
	}
}

func startConnected(t *testing.T, s *fakeServer) *Client {
	t.Helper()
	c := newTestClient(t, s)
	c.Start()
	waitFor(t, "connection", func() bool { return c.State().Connected })
	return c
}

func api(t *testing.T, c *Client, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := c.API(ctx, cmd)
	if err != nil {
		t.Fatalf("API(%q): %v", cmd, err)
	}
	return out
}

func TestAuthSuccess(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(16)
	defer cancel()
	c.Start()
	c.Start()

	m := waitMsg(t, msgs, "connected state", isState(true))
	if m.Headers["error"] != "" {
		t.Errorf("error header = %q, want empty", m.Headers["error"])
	}
	s.expectCmds("event json HEARTBEAT")
	st := c.State()
	if !st.Connected || st.LastError != "" || st.Since.IsZero() {
		t.Errorf("state = %+v", st)
	}
	if n := s.accepted.Load(); n != 1 {
		t.Errorf("accepted %d connections, want 1 (Start twice must be a no-op)", n)
	}
}

func TestAuthFailure(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer) { s.password = "other" })
	c := newTestClient(t, s)
	c.Start()
	waitFor(t, "auth error in state", func() bool { return c.State().LastError == ErrAuth.Error() })
	waitFor(t, "auth retry", func() bool { return s.authAttempts.Load() >= 3 })
	if c.State().Connected {
		t.Error("connected after failed auth")
	}
	if _, err := c.API(context.Background(), "status"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("API error = %v, want ErrNotConnected", err)
	}
}

func TestRudeRejection(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer) { s.reject = "Access Denied, go away.\n" })
	c := newTestClient(t, s)
	c.Start()
	waitFor(t, "rejection in state", func() bool { return c.State().LastError == "Access Denied, go away." })
}

func TestAPIRoundTrip(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	if got := api(t, c, "echo hello world"); got != "hello world" {
		t.Errorf("got %q", got)
	}
	if got := api(t, c, "multi"); got != "line one\nline two\n\nline four\n" {
		t.Errorf("multi-line body = %q", got)
	}
}

func TestAPIErrBodyVerbatim(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	if got := api(t, c, "fail"); got != "-ERR fail Command not found!\n" {
		t.Errorf("got %q", got)
	}
}

func TestAPIConcurrentOrdering(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	// Never read: interleaved events must not block the reader.
	_, cancel := c.Subscribe(0)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 300)
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := "echo " + strconv.Itoa(i)
			if i%50 == 0 {
				cmd = "event-first"
			}
			out, err := c.API(context.Background(), cmd)
			want := strconv.Itoa(i)
			if cmd == "event-first" {
				want = "after event"
			}
			if err != nil || out != want {
				errs <- fmt.Errorf("API(%q) = %q, %v; want %q", cmd, out, err, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestEventDelivery(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(16)
	defer cancel()
	c.Start()
	fc := s.nextConn()
	fc.write(eventFrame(`{"Event-Name":"HEARTBEAT","Idle-CPU":"98.5","Session-Count":"0",` +
		`"Number":5,"Multi":["a","b"],"_body":"the body"}`))

	m := waitMsg(t, msgs, "event", func(m Message) bool { return m.Kind == KindEvent })
	want := map[string]string{
		"Event-Name": "HEARTBEAT", "Idle-CPU": "98.5", "Session-Count": "0",
		"Number": "5", "Multi": `["a","b"]`,
	}
	if len(m.Headers) != len(want) {
		t.Errorf("headers = %v, want %v", m.Headers, want)
	}
	for k, v := range want {
		if m.Headers[k] != v {
			t.Errorf("header %s = %q, want %q", k, m.Headers[k], v)
		}
	}
	if m.Body != "the body" {
		t.Errorf("body = %q", m.Body)
	}

	// An event arriving between a request and its reply must not be taken
	// as the reply.
	if got := api(t, c, "event-first"); got != "after event" {
		t.Errorf("event-first = %q", got)
	}
	m = waitMsg(t, msgs, "interleaved event", func(m Message) bool { return m.Kind == KindEvent })
	if m.Headers["Event-Name"] != "CUSTOM" || m.Body != "not a reply" {
		t.Errorf("interleaved event = %+v", m)
	}
}

func TestLogDelivery(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	fc := s.nextConn()
	msgs, cancel := c.Subscribe(16)
	defer cancel()

	c.SetLogLevel(7)
	s.expectCmds("log 7")
	fc.write(logFrame(7, "hello from the log\n"))
	m := waitMsg(t, msgs, "log", func(m Message) bool { return m.Kind == KindLog })
	if m.Body != "hello from the log\n" {
		t.Errorf("body = %q", m.Body)
	}
	for k, v := range map[string]string{"Log-Level": "7", "Log-File": "switch_core.c", "Log-Func": "test_func", "Log-Line": "42", "Text-Channel": "3", "User-Data": ""} {
		if got, ok := m.Headers[k]; !ok || got != v {
			t.Errorf("header %s = %q (present %v), want %q", k, got, ok, v)
		}
	}

	c.SetLogLevel(-1)
	s.expectCmds("nolog")
	c.SetLogLevel(99)
	s.expectCmds("log 7")
}

func TestSetLogLevelWhileDisconnected(t *testing.T) {
	c := NewClient("127.0.0.1:1", testPassword)
	defer c.Close()
	done := make(chan struct{})
	go func() {
		c.SetLogLevel(3)
		c.SetLogLevel(4)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SetLogLevel blocked while not connected")
	}
}

func TestReconnectReappliesSetup(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	s.nextConn()
	waitMsg(t, msgs, "first connect", isState(true))
	s.expectCmds("event json HEARTBEAT")
	c.SetLogLevel(6)
	s.expectCmds("log 6")

	s.dropAll()
	m := waitMsg(t, msgs, "disconnect", isState(false))
	if m.Headers["error"] == "" {
		t.Error("disconnect state carries no error")
	}
	s.nextConn()
	waitMsg(t, msgs, "reconnect", isState(true))
	s.expectCmds("event json HEARTBEAT", "log 6")
	if got := api(t, c, "echo again"); got != "again" {
		t.Errorf("got %q", got)
	}
}

func TestDisconnectNotice(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	fc := s.nextConn()
	waitMsg(t, msgs, "connect", isState(true))
	body := "Disconnected, goodbye.\nSee you at ClueCon!\n"
	fc.write(fmt.Sprintf("Content-Type: text/disconnect-notice\nContent-Length: %d\n\n%s", len(body), body))
	m := waitMsg(t, msgs, "disconnect", isState(false))
	if !strings.Contains(m.Headers["error"], "goodbye") {
		t.Errorf("error = %q", m.Headers["error"])
	}
	s.nextConn()
	waitMsg(t, msgs, "reconnect", isState(true))
}

func TestAPITimeoutClosesConnection(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	s.nextConn()
	waitMsg(t, msgs, "connect", isState(true))

	ctx, cancelCtx := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCtx()
	if _, err := c.API(ctx, "sleep"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	waitMsg(t, msgs, "disconnect after timeout", isState(false))
	s.nextConn()
	waitMsg(t, msgs, "reconnect", isState(true))
	if got := api(t, c, "echo fresh"); got != "fresh" {
		t.Errorf("got %q", got)
	}
}

func TestInvalidCommand(t *testing.T) {
	c := NewClient("127.0.0.1:1", testPassword)
	defer c.Close()
	if _, err := c.API(context.Background(), "status"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("not connected: err = %v", err)
	}

	s := newFakeServer(t, nil)
	c = startConnected(t, s)
	s.expectCmds("event json HEARTBEAT")
	for _, cmd := range []string{"status\n\nexit", "status\rexit", "\n"} {
		if _, err := c.API(context.Background(), cmd); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("API(%q) err = %v, want ErrInvalidCommand", cmd, err)
		}
	}
	api(t, c, "echo marker")
	if got := <-s.cmds; got != "api echo marker" {
		t.Errorf("server received %q before the marker", got)
	}
}

func TestSubscribeCancelAndClose(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	fc := s.nextConn()

	a, cancelA := c.Subscribe(1)
	b, _ := c.Subscribe(1)
	cancelA()
	cancelA()
	if _, ok := <-a; ok {
		t.Error("cancelled channel still open")
	}

	// A full subscriber must not stall delivery or the reader.
	for i := 0; i < 50; i++ {
		fc.write(eventFrame(`{"Event-Name":"HEARTBEAT"}`))
	}
	if got := api(t, c, "echo still reading"); got != "still reading" {
		t.Errorf("got %q", got)
	}

	c.Close()
	c.Close()
	timeout := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-b:
		case <-timeout:
			t.Fatal("subscriber channel not closed by Close")
		}
	}
	late, cancelLate := c.Subscribe(1)
	if _, ok := <-late; ok {
		t.Error("Subscribe after Close returned an open channel")
	}
	cancelLate()
	if st := c.State(); st.Connected {
		t.Errorf("state after Close = %+v", st)
	}
	if _, err := c.API(context.Background(), "status"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("API after Close err = %v", err)
	}
	c.Start() // no-op after Close
}

func TestCloseWithPendingAPI(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	errc := make(chan error, 1)
	go func() {
		_, err := c.API(context.Background(), "sleep")
		errc <- err
	}()
	s.expectCmds("api sleep")
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with a pending API call")
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrNotConnected) {
			t.Errorf("pending API err = %v, want ErrNotConnected", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending API call not released by Close")
	}
}

func TestCloseDuringHandshake(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer) { s.silent = true })
	c := newTestClient(t, s)
	c.Start()
	waitFor(t, "accept", func() bool { return s.accepted.Load() == 1 })
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung during handshake")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	msgs, _ := c.Subscribe(4)
	c.SetLogLevel(5)
	s.expectCmds("log 5")
	api(t, c, "echo x")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	c.API(ctx, "sleep")
	cancel()
	c.Close()
	s.close()
	for range msgs {
	}
	waitFor(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= before })
}
