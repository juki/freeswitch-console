package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/store"
)

func TestVerifySlotsCap(t *testing.T) {
	h := newHarness(t)
	h.srv.verifyWait = 50 * time.Millisecond
	for i := 0; i < cap(h.srv.verifySlots); i++ {
		h.srv.verifySlots <- struct{}{}
	}
	start := time.Now()
	r := h.loginAs("admin", adminPW)
	expect(t, r, 503, `{"error":"too many password checks at once; try again in a moment"}`)
	if r.header.Get("Retry-After") != "1" || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("Retry-After %q after %v", r.header.Get("Retry-After"), time.Since(start))
	}
	// A refused attempt is not charged to the rate limit.
	for i := 0; i < 6; i++ {
		expect(t, h.loginAs("admin", "wrong"), 503, "")
	}
	<-h.srv.verifySlots
	h.login("admin", adminPW)
	for len(h.srv.verifySlots) > 0 {
		<-h.srv.verifySlots
	}

	// Many concurrent sign-ins for unknown names from many addresses never
	// hash more than the cap at once.
	h.srv.verifyWait = 5 * time.Second
	var mu sync.Mutex
	peak := 0
	stop, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			peak = max(peak, len(h.srv.verifySlots))
			mu.Unlock()
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.loginFrom(fmt.Sprintf("192.0.2.%d:1", i+1), "nobody-"+string(rune('a'+i)), "x")
		}()
	}
	wg.Wait()
	close(stop)
	<-watched
	if peak > cap(h.srv.verifySlots) || len(h.srv.verifySlots) != 0 {
		t.Fatalf("peak %d, left %d", peak, len(h.srv.verifySlots))
	}
}

func TestVerifyPerAddress(t *testing.T) {
	h := newHarness(t)
	h.srv.verifyWait = 5 * time.Second
	for i := 0; i < cap(h.srv.verifySlots); i++ {
		h.srv.verifySlots <- struct{}{}
	}
	inFlight := func(ip string) int {
		h.srv.verifyIPMu.Lock()
		defer h.srv.verifyIPMu.Unlock()
		return h.srv.verifyIP[ip]
	}
	// Two sign-ins from one address wait for a slot; a third from there is
	// refused at once, while other addresses still queue.
	codes := make(chan int, 3)
	for _, from := range []string{"192.0.2.9:1", "192.0.2.9:2", "198.51.100.1:1"} {
		go func() { codes <- h.loginFrom(from, "admin", "wrong") }()
	}
	waitFor(t, "waiting sign-ins", func() bool { return inFlight("192.0.2.9") == 2 && inFlight("198.51.100.1") == 1 })
	start := time.Now()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"viewer","password":"`+viewerPW+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.9:3"
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 || strings.TrimSpace(rec.Body.String()) != `{"error":"too many password checks at once; try again in a moment"}` || rec.Header().Get("Retry-After") != "1" || time.Since(start) > time.Second {
		t.Fatalf("third sign-in: %d %s after %v", rec.Code, rec.Body, time.Since(start))
	}
	for i := 0; i < cap(h.srv.verifySlots); i++ {
		<-h.srv.verifySlots
	}
	for i := 0; i < 3; i++ {
		if c := <-codes; c != 401 {
			t.Fatalf("waiting sign-in: %d", c)
		}
	}
	h.srv.verifyIPMu.Lock()
	left := len(h.srv.verifyIP)
	h.srv.verifyIPMu.Unlock()
	if left != 0 {
		t.Fatalf("%d addresses left in the table", left)
	}
	if c := h.loginFrom("192.0.2.9:4", "viewer", viewerPW); c != 200 {
		t.Fatalf("after the checks: %d", c)
	}
}

func TestChangePasswordRateLimit(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	h.addUser("admin", store.RoleAdmin)
	tok := h.login("admin", userPW)
	for i := 0; i < 4; i++ {
		expect(t, h.do("POST", "/api/me/password", tok, `{"current":"wrong-password","new":"next-password-1"}`), 400, "")
	}
	// A right guess clears the count.
	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`","new":"next-password-1"}`), 204, "")
	for i := 0; i < 5; i++ {
		expect(t, h.do("POST", "/api/me/password", tok, `{"current":"wrong-password","new":"next-password-2"}`), 400, `{"error":"current password is wrong"}`)
	}
	r := h.do("POST", "/api/me/password", tok, `{"current":"next-password-1","new":"next-password-2"}`)
	expect(t, r, 429, `{"error":"too many failed attempts; try again in 60s"}`)
	if r.header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After %q", r.header.Get("Retry-After"))
	}
	// The same limiter guards the sign-in of that account from this address.
	expect(t, h.loginAs("admin", "next-password-1"), 429, "")
	if c := h.loginFrom("192.0.2.1:1", "admin", "next-password-1"); c != 200 {
		t.Fatalf("other address: %d", c)
	}
}

func TestInstanceAPILimit(t *testing.T) {
	h := newHarness(t)
	viewer := h.login("viewer", viewerPW)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	f := h.fake(0)
	hold := make(chan struct{})
	f.set(func(f *fakeSwitch) { f.hold = hold })
	h.instance(h.id).sw.(*limitedSwitch).wait = 100 * time.Millisecond

	codes := make(chan int, apiSlots)
	for i := 0; i < apiSlots; i++ {
		go func() { codes <- h.do("GET", p+"/channels", viewer, "").code }()
	}
	waitFor(t, "calls in flight", func() bool { sent, _, _, _ := f.snapshot(); return len(sent) == apiSlots })
	// The switch never answers; further requests are refused after the
	// wait without reaching the connection.
	busy := `{"error":"too many requests to this instance at once; try again in a moment"}`
	for _, rq := range []struct{ method, path, token, body string }{
		{"GET", p + "/channels", viewer, ""},
		{"GET", p + "/status", viewer, ""},
		{"GET", p + "/config/sofia.conf", viewer, ""},
	} {
		start := time.Now()
		expect(t, h.do(rq.method, rq.path, rq.token, rq.body), 503, busy)
		if d := time.Since(start); d < 100*time.Millisecond {
			t.Fatalf("%s refused after %v", rq.path, d)
		}
	}
	if sent, _, _, _ := f.snapshot(); len(sent) != apiSlots || !f.State().Connected {
		t.Fatalf("sent %d, connected %v", len(sent), f.State().Connected)
	}
	// The console and the actions run with Background, which has slots of
	// its own.
	expect(t, h.do("POST", p+"/console", admin, `{"command":"status"}`), 200, "")
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadxml"}`), 200, "")
	close(hold)
	for i := 0; i < apiSlots; i++ {
		if c := <-codes; c != 200 {
			t.Fatalf("held call: %d", c)
		}
	}
	expect(t, h.do("GET", p+"/channels", viewer, ""), 200, "")
}

func TestInstanceBackgroundLimit(t *testing.T) {
	h := newHarness(t)
	viewer := h.login("viewer", viewerPW)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	f := h.fake(0)
	hold := make(chan struct{})
	f.set(func(f *fakeSwitch) { f.bgHold = hold })
	h.instance(h.id).sw.(*limitedSwitch).wait = 100 * time.Millisecond

	codes := make(chan int, backgroundSlots)
	for i := range backgroundSlots {
		rq := [2]string{"/console", `{"command":"originate x"}`}
		if i%2 == 1 {
			rq = [2]string{"/actions", `{"action":"reloadxml"}`}
		}
		go func() { codes <- h.do("POST", p+rq[0], admin, rq[1]).code }()
	}
	waitFor(t, "background calls in flight", func() bool { sent, _, _, _ := f.snapshot(); return len(sent) == backgroundSlots })
	busy := `{"error":"too many requests to this instance at once; try again in a moment"}`
	for _, rq := range [][2]string{{"/console", `{"command":"status"}`}, {"/actions", `{"action":"reloadacl"}`}} {
		start := time.Now()
		expect(t, h.do("POST", p+rq[0], admin, rq[1]), 503, busy)
		if d := time.Since(start); d < 100*time.Millisecond {
			t.Fatalf("%s refused after %v", rq[0], d)
		}
	}
	// API calls have slots of their own.
	expect(t, h.do("GET", p+"/channels", viewer, ""), 200, "")
	if sent, _, _, _ := f.snapshot(); len(sent) != backgroundSlots+1 || !f.State().Connected {
		t.Fatalf("sent %q, connected %v", sent, f.State().Connected)
	}
	close(hold)
	for range backgroundSlots {
		if c := <-codes; c != 200 {
			t.Fatalf("held call: %d", c)
		}
	}
	expect(t, h.do("POST", p+"/console", admin, `{"command":"status"}`), 200, "")
}

// ended reports whether the stream ends within d.
func ended(ch <-chan sseEvent, d time.Duration) bool {
	timeout := time.After(d)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

func TestStreamEndsWithSession(t *testing.T) {
	h := newHarness(t)
	h.srv.streamCheck = 20 * time.Millisecond
	admin := h.login("admin", adminPW)
	open := func(token, query string) <-chan sseEvent {
		t.Helper()
		ch, cancel := h.openStream(token, query)
		t.Cleanup(cancel)
		next(t, ch)
		return ch
	}
	healthy := open(admin, "?loglevel=7")

	// Disabled.
	id := h.addUser("op1", store.RoleOperator)
	ch := open(h.login("op1", userPW), "?loglevel=7")
	expect(t, h.do("PUT", "/api/users/"+id, admin, `{"disabled":true}`), 200, "")
	if !ended(ch, time.Second) {
		t.Fatal("stream of a disabled account still open")
	}

	// Demoted to viewer, here without the session ending: the stream with
	// log lines ends, the one without stays.
	id = h.addUser("op2", store.RoleOperator)
	tok := h.login("op2", userPW)
	logs, plain := open(tok, "?loglevel=7"), open(tok, "")
	u, _ := h.st.User(id)
	u.Role = store.RoleViewer
	if err := h.st.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	if !ended(logs, time.Second) {
		t.Fatal("log stream of a demoted account still open")
	}
	if ended(plain, 200*time.Millisecond) {
		t.Fatal("stream without log lines ended on demotion")
	}

	// Signed out.
	h.addUser("op3", store.RoleOperator)
	tok = h.login("op3", userPW)
	ch = open(tok, "?loglevel=7")
	expect(t, h.do("POST", "/api/logout", tok, ""), 204, "")
	if !ended(ch, time.Second) {
		t.Fatal("stream still open after sign-out")
	}

	if ended(healthy, 200*time.Millisecond) {
		t.Fatal("healthy stream ended")
	}
}

func TestStreamCaps(t *testing.T) {
	h := newHarness(t)
	h.srv.maxSessionStreams, h.srv.maxStreams = 2, 3
	path := "/api/instances/" + h.id + "/stream"
	// Two sessions of one account count apart.
	a, b := h.login("admin", adminPW), h.login("admin", adminPW)
	viewer := h.login("viewer", viewerPW)
	_, cancelA1 := h.openStream(a, "")
	defer cancelA1()
	_, cancelA2 := h.openStream(a, "")
	defer cancelA2()
	expect(t, h.do("GET", path, a, ""), 429, `{"error":"too many event streams open in this session; close another console tab"}`)
	_, cancelB := h.openStream(b, "")
	defer cancelB()
	expect(t, h.do("GET", path, viewer, ""), 429, `{"error":"too many event streams open on the server; try again later"}`)

	// A stream that ends frees its place.
	cancelA1()
	waitFor(t, "stream released", func() bool {
		h.srv.streamsMu.Lock()
		defer h.srv.streamsMu.Unlock()
		return h.srv.openStreams == 2 && h.srv.sessionStreams[a] == 1
	})
	_, cancelV := h.openStream(viewer, "")
	defer cancelV()
}
