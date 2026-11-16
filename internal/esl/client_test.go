package esl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPassword = "ClueCon"
	slowReply    = 250 * time.Millisecond // delay of "api slow" and of the "bgapi slow" result
)

// fakeServer speaks enough of mod_event_socket's inbound protocol for tests.
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	password string
	silent   bool   // accept but never send auth/request
	reject   string // answer with text/rude-rejection carrying this body
	mute     int32  // the first mute connections authenticate, then never answer

	cmds         chan string    // every command received after auth
	conns        chan *fakeConn // every authenticated connection
	authAttempts atomic.Int32
	accepted     atomic.Int32
	lateSent     atomic.Bool // the result of "bgapi late" has been written

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
		n := s.accepted.Add(1)
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
			s.serve(nc, n <= s.mute)
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

// jobEvent is the BACKGROUND_JOB event FreeSWITCH fires when job id ends.
func jobEvent(id, command, body string) string {
	b, _ := json.Marshal(map[string]string{"Event-Name": "BACKGROUND_JOB", "Job-UUID": id, "Job-Command": command, "_body": body})
	return eventFrame(string(b))
}

func logFrame(level int, text string) string {
	return fmt.Sprintf("Content-Type: log/data\nContent-Length: %d\nLog-Level: %d\nText-Channel: 3\n"+
		"Log-File: switch_core.c\nLog-Func: test_func\nLog-Line: 42\nUser-Data: \n\n%s", len(text), level, text)
}

func (s *fakeServer) serve(nc net.Conn, mute bool) {
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
		if !mute {
			s.handle(fc, cmd)
		}
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
	case cmd == "api slow":
		// Blocks reading, like FreeSWITCH running one connection's commands
		// one after another.
		time.Sleep(slowReply)
		fc.write(apiResponse("done"))
	case cmd == "api uptime":
		fc.write(apiResponse("42\n"))
	case cmd == "api event-first":
		fc.write(eventFrame(`{"Event-Name":"CUSTOM","_body":"not a reply"}`))
		fc.write(apiResponse("after event"))
	case strings.HasPrefix(cmd, "bgapi "):
		s.background(fc, cmd)
	case strings.HasPrefix(cmd, "event json "):
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

// background answers "bgapi <job>" with a Job-UUID header line like
// FreeSWITCH: the reply at once, then the job's result as a BACKGROUND_JOB
// event. Slow jobs end in a goroutine, so the connection keeps being read.
func (s *fakeServer) background(fc *fakeConn, cmd string) {
	line, header, _ := strings.Cut(cmd, "\n")
	job := strings.TrimPrefix(line, "bgapi ")
	id, ok := strings.CutPrefix(header, "Job-UUID: ")
	switch {
	case !ok:
		fc.write(reply("-ERR no Job-UUID"))
		return
	case job == "denied":
		fc.write(reply("-ERR permission denied"))
		return
	}
	fc.write("Content-Type: command/reply\nReply-Text: +OK Job-UUID: " + id + "\nJob-UUID: " + id + "\n\n")
	switch {
	case strings.HasPrefix(job, "echo "):
		fc.write(jobEvent(id, "echo", strings.TrimPrefix(job, "echo ")))
	case job == "multi":
		fc.write(jobEvent(id, "multi", "line one\nline two\n\nline four\n"))
	case job == "slow":
		go func() {
			time.Sleep(slowReply)
			fc.write(jobEvent(id, "slow", "slow done"))
		}()
	case job == "late":
		go func() {
			time.Sleep(slowReply)
			fc.write(jobEvent(id, "late", "too late"))
			s.lateSent.Store(true)
		}()
	case job == "other-first":
		// Another ESL client's job ends first.
		fc.write(jobEvent("00000000-0000-4000-8000-000000000000", "status", "not mine"))
		fc.write(jobEvent(id, "other-first", "mine"))
	case job == "never":
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
	c := NewClient(s.addr(), testPassword, "HEARTBEAT")
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
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT")
	st := c.State()
	if !st.Connected || st.LastError != "" || st.Since.IsZero() {
		t.Errorf("state = %+v", st)
	}
	if n := s.accepted.Load(); n != 1 {
		t.Errorf("accepted %d connections, want 1 (Start twice must be a no-op)", n)
	}
}

// TestEventSubscription checks the subscription line: BACKGROUND_JOB, then
// the events given to NewClient.
func TestEventSubscription(t *testing.T) {
	for _, events := range [][]string{{"HEARTBEAT", "CHANNEL_HANGUP_COMPLETE"}, {"HEARTBEAT"}, nil} {
		s := newFakeServer(t, nil)
		c := NewClient(s.addr(), testPassword, events...)
		t.Cleanup(c.Close)
		c.Start()
		waitFor(t, "connection", func() bool { return c.State().Connected })
		api(t, c, "echo done")
		var got []string
		for range len(s.cmds) {
			got = append(got, <-s.cmds)
		}
		want := []string{"event json " + strings.Join(append([]string{"BACKGROUND_JOB"}, events...), " "), "api echo done"}
		if !slices.Equal(got, want) {
			t.Errorf("events %q: commands %q, want %q", events, got, want)
		}
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
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT")
	c.SetLogLevel(6)
	s.expectCmds("log 6")

	s.dropAll()
	m := waitMsg(t, msgs, "disconnect", isState(false))
	if m.Headers["error"] == "" {
		t.Error("disconnect state carries no error")
	}
	s.nextConn()
	waitMsg(t, msgs, "reconnect", isState(true))
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT", "log 6")
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
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT")
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

func probingClient(t *testing.T, s *fakeServer, after, timeout time.Duration) *Client {
	t.Helper()
	c := newTestClient(t, s)
	c.probeAfter, c.probeTimeout = after, timeout
	return c
}

// noDisconnect fails the test if msgs holds a disconnected state.
func noDisconnect(t *testing.T, msgs <-chan Message) {
	t.Helper()
	for {
		select {
		case m := <-msgs:
			if isState(false)(m) {
				t.Fatalf("disconnected: %q", m.Headers["error"])
			}
		default:
			return
		}
	}
}

func TestProbeSilentPeer(t *testing.T) {
	const after, timeout = 60 * time.Millisecond, 60 * time.Millisecond
	s := newFakeServer(t, func(s *fakeServer) { s.mute = 1 })
	c := probingClient(t, s, after, timeout)
	c.backoff = 300 * time.Millisecond // keeps the failed state observable
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	waitMsg(t, msgs, "connect", isState(true))
	start := time.Now()

	// The unanswered setup command must not hold the probe back.
	m := waitMsg(t, msgs, "disconnect", isState(false))
	elapsed := time.Since(start)
	const want = "esl: no answer from FreeSWITCH"
	if m.Headers["error"] != want {
		t.Errorf("error header = %q, want %q", m.Headers["error"], want)
	}
	if st := c.State(); st.Connected || st.LastError != want {
		t.Errorf("state = %+v, want LastError %q", st, want)
	}
	if elapsed < after || elapsed > after+timeout+time.Second {
		t.Errorf("disconnected after %v, want about %v", elapsed, after+timeout)
	}
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT", "api uptime")

	waitMsg(t, msgs, "reconnect", isState(true))
	if got := api(t, c, "echo alive"); got != "alive" {
		t.Errorf("got %q", got)
	}
}

func TestProbeQuietPeer(t *testing.T) {
	const after = 40 * time.Millisecond
	s := newFakeServer(t, nil)
	c := probingClient(t, s, after, 40*time.Millisecond)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	waitMsg(t, msgs, "connect", isState(true))
	start := time.Now()
	for i := 0; i < 4; i++ {
		s.expectCmds("api uptime")
	}
	// Each reply resets the idle timer, so probes are spaced by probeAfter.
	if elapsed := time.Since(start); elapsed < 3*after {
		t.Errorf("4 probes within %v", elapsed)
	}
	noDisconnect(t, msgs)
	if n := s.accepted.Load(); n != 1 || !c.State().Connected {
		t.Errorf("accepted %d connections, state %+v", n, c.State())
	}
}

func TestProbeChattyPeer(t *testing.T) {
	s := newFakeServer(t, nil)
	c := probingClient(t, s, 80*time.Millisecond, 80*time.Millisecond)
	c.Start()
	fc := s.nextConn()
	for end := time.Now().Add(400 * time.Millisecond); time.Now().Before(end); {
		fc.write(eventFrame(`{"Event-Name":"HEARTBEAT"}`))
		time.Sleep(10 * time.Millisecond)
	}
	for {
		select {
		case cmd := <-s.cmds:
			if cmd == "api uptime" {
				t.Fatal("probed a peer that sends events")
			}
		default:
			if !c.State().Connected {
				t.Errorf("state = %+v", c.State())
			}
			return
		}
	}
}

func TestProbeWaitsForPendingAPI(t *testing.T) {
	const after, timeout = 40 * time.Millisecond, 40 * time.Millisecond
	if slowReply <= 2*(after+timeout) {
		t.Fatal("slowReply too short for this test")
	}
	s := newFakeServer(t, nil)
	c := probingClient(t, s, after, timeout)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	waitMsg(t, msgs, "connect", isState(true))
	// The fake reads nothing while "slow" runs, so a probe sent meanwhile
	// would time out and drop the connection, failing the call.
	if got := api(t, c, "slow"); got != "done" {
		t.Errorf("got %q", got)
	}
	noDisconnect(t, msgs)
	if n := s.accepted.Load(); n != 1 || !c.State().Connected {
		t.Errorf("accepted %d connections, state %+v", n, c.State())
	}
}

func background(t *testing.T, c *Client, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := c.Background(ctx, cmd)
	if err != nil {
		t.Fatalf("Background(%q): %v", cmd, err)
	}
	return out
}

// jobsWaiting returns the number of Background calls the current connection
// holds a waiter for.
func jobsWaiting(c *Client) int {
	c.mu.Lock()
	cn := c.cur
	c.mu.Unlock()
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return len(cn.jobs)
}

var bgapiLine = regexp.MustCompile(`^bgapi echo hello world\nJob-UUID: [0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestBackgroundRoundTrip(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT")
	if got := background(t, c, "echo hello world"); got != "hello world" {
		t.Errorf("got %q", got)
	}
	if got := <-s.cmds; !bgapiLine.MatchString(got) {
		t.Errorf("server received %q", got)
	}
	if got := background(t, c, "multi"); got != "line one\nline two\n\nline four\n" {
		t.Errorf("multi-line body = %q", got)
	}
	// A refused bgapi is returned like an -ERR body.
	if got := background(t, c, "denied"); got != "-ERR permission denied" {
		t.Errorf("denied: got %q", got)
	}
	if n := jobsWaiting(c); n != 0 {
		t.Errorf("%d waiters left", n)
	}
}

// TestBackgroundConcurrent runs two slow jobs and an API call at once: the
// API call does not wait for the jobs.
func TestBackgroundConcurrent(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	start := time.Now()
	var wg sync.WaitGroup
	outs := make([]string, 2)
	for i := range outs {
		wg.Go(func() {
			out, err := c.Background(context.Background(), "slow")
			outs[i] = fmt.Sprint(out, err)
		})
	}
	waitFor(t, "both jobs sent", func() bool { return jobsWaiting(c) == 2 })
	apiStart := time.Now()
	if got := api(t, c, "echo quick"); got != "quick" {
		t.Errorf("API got %q", got)
	}
	if d := time.Since(apiStart); d > slowReply/2 {
		t.Errorf("API call took %v behind two background jobs", d)
	}
	wg.Wait()
	for i, out := range outs {
		if out != "slow done<nil>" {
			t.Errorf("job %d: %q", i, out)
		}
	}
	if d := time.Since(start); d > 2*slowReply {
		t.Errorf("two jobs took %v, want about %v", d, slowReply)
	}
}

func TestBackgroundDeadlineKeepsConnection(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	waitMsg(t, msgs, "connect", isState(true))

	ctx, cancelCtx := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCtx()
	if _, err := c.Background(ctx, "never"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if n := jobsWaiting(c); n != 0 {
		t.Errorf("%d waiters left after the deadline", n)
	}
	if got := api(t, c, "echo still here"); got != "still here" {
		t.Errorf("API got %q", got)
	}
	if got := background(t, c, "echo again"); got != "again" {
		t.Errorf("Background got %q", got)
	}
	noDisconnect(t, msgs)
	if n := s.accepted.Load(); n != 1 {
		t.Errorf("accepted %d connections, want 1", n)
	}
}

// TestBackgroundLateResult gives up on a job whose result arrives later: the
// result is dropped, not delivered to the next call or to subscribers.
func TestBackgroundLateResult(t *testing.T) {
	s := newFakeServer(t, nil)
	c := newTestClient(t, s)
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	c.Start()
	waitMsg(t, msgs, "connect", isState(true))

	ctx, cancelCtx := context.WithTimeout(context.Background(), slowReply/5)
	defer cancelCtx()
	if _, err := c.Background(ctx, "late"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	waitFor(t, "the late result", s.lateSent.Load)
	// The reply to this call comes after the late result on the wire.
	if got := api(t, c, "echo after"); got != "after" {
		t.Errorf("API got %q", got)
	}
	if got := background(t, c, "echo next"); got != "next" {
		t.Errorf("next Background got %q", got)
	}
	for len(msgs) > 0 {
		if m := <-msgs; m.Kind == KindEvent || isState(false)(m) {
			t.Errorf("subscriber got %+v", m)
		}
	}
	if n := s.accepted.Load(); n != 1 || !c.State().Connected {
		t.Errorf("accepted %d connections, state %+v", n, c.State())
	}
}

// TestBackgroundOtherJobs ignores the results of other clients' jobs and
// delivers no BACKGROUND_JOB event to subscribers.
func TestBackgroundOtherJobs(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	fc := s.nextConn()
	msgs, cancel := c.Subscribe(64)
	defer cancel()
	if got := background(t, c, "other-first"); got != "mine" {
		t.Errorf("got %q", got)
	}
	fc.write(jobEvent("11111111-1111-4111-8111-111111111111", "status", "nobody waits"))
	fc.write(eventFrame(`{"Event-Name":"HEARTBEAT"}`))
	m := waitMsg(t, msgs, "event", func(m Message) bool { return m.Kind == KindEvent })
	if m.Headers["Event-Name"] != "HEARTBEAT" {
		t.Errorf("subscriber got %+v before the heartbeat", m)
	}
	if !c.State().Connected {
		t.Errorf("state %+v", c.State())
	}
}

func TestBackgroundConnectionLoss(t *testing.T) {
	s := newFakeServer(t, nil)
	c := startConnected(t, s)
	errc := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.Background(context.Background(), "never")
			errc <- err
		}()
	}
	waitFor(t, "both jobs sent", func() bool { return jobsWaiting(c) == 2 })
	s.dropAll()
	for range 2 {
		select {
		case err := <-errc:
			if !errors.Is(err, ErrNotConnected) {
				t.Errorf("err = %v, want ErrNotConnected", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("waiting job not released by the connection loss")
		}
	}
}

func TestBackgroundInvalidCommand(t *testing.T) {
	c := NewClient("127.0.0.1:1", testPassword)
	defer c.Close()
	if _, err := c.Background(context.Background(), "status"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("not connected: err = %v", err)
	}

	s := newFakeServer(t, nil)
	c = startConnected(t, s)
	s.expectCmds("event json BACKGROUND_JOB HEARTBEAT")
	for _, cmd := range []string{"status\n\nexit", "status\rexit", "\n", "echo x\nJob-UUID: 1"} {
		if _, err := c.Background(context.Background(), cmd); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("Background(%q) err = %v, want ErrInvalidCommand", cmd, err)
		}
	}
	api(t, c, "echo marker")
	if got := <-s.cmds; got != "api echo marker" {
		t.Errorf("server received %q before the marker", got)
	}
}
