package server

import (
	"sync"
	"testing"
	"time"

	"github.com/juki/fsdeck/internal/store"
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

	// Many concurrent sign-ins for unknown names never hash more than the
	// cap at once.
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
			h.loginFrom("192.0.2.9:1", "nobody-"+string(rune('a'+i)), "x")
		}()
	}
	wg.Wait()
	close(stop)
	<-watched
	if peak > cap(h.srv.verifySlots) || len(h.srv.verifySlots) != 0 {
		t.Fatalf("peak %d, left %d", peak, len(h.srv.verifySlots))
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
