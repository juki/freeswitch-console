package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func withBacklog(lines, level int) func(*Config) {
	return func(c *Config) { c.LogBacklog, c.LogBacklogLevel = lines, level }
}

func (i *instance) lastSeq() uint64 {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	return i.seq
}

func decodeLog(t *testing.T, ev sseEvent) logEvent {
	t.Helper()
	if ev.name != "log" {
		t.Fatalf("want a log event, got %+v", ev)
	}
	var l logEvent
	if err := json.Unmarshal([]byte(ev.data), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// replay reads the state event, the replayed lines and the backlog event.
func replay(t *testing.T, ch <-chan sseEvent) []logEvent {
	t.Helper()
	if ev := next(t, ch); ev.name != "state" {
		t.Fatalf("first event %+v", ev)
	}
	var lines []logEvent
	for {
		ev := next(t, ch)
		if ev.name == "backlog" {
			var b struct{ Lines int }
			json.Unmarshal([]byte(ev.data), &b)
			if b.Lines != len(lines) || ev.data != fmt.Sprintf(`{"lines":%d}`, len(lines)) {
				t.Fatalf("backlog event %s after %d lines", ev.data, len(lines))
			}
			return lines
		}
		lines = append(lines, decodeLog(t, ev))
	}
}

func texts(lines []logEvent) string {
	var out []string
	for _, l := range lines {
		out = append(out, fmt.Sprintf("%d:%d:%s", l.Seq, l.Level, l.Text))
	}
	return strings.Join(out, " ")
}

func TestBacklogReplay(t *testing.T) {
	h := newHarness(t, withBacklog(4, 6))
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	inst := h.instance(h.id)
	for _, m := range [][2]string{{"3", "a"}, {"7", "not kept"}, {"6", "b"}, {"x", "bad level"}, {"4", "c"}, {"6", "d"}, {"2", "e"}} {
		f.publish(logMsg(m[0], m[1]))
	}
	waitFor(t, "stored lines", func() bool { return inst.lastSeq() == 5 })

	// The ring keeps the last four, oldest first.
	ch, cancel := h.openStream(admin, "?loglevel=7")
	defer cancel()
	if got := texts(replay(t, ch)); got != "2:6:b 3:4:c 4:6:d 5:2:e" {
		t.Fatalf("replayed %s", got)
	}
	// Live: a line above the backlog level is not kept and has seq 0.
	f.publish(logMsg("7", "debug"))
	f.publish(logMsg("5", "notice"))
	if got := texts([]logEvent{decodeLog(t, next(t, ch)), decodeLog(t, next(t, ch))}); got != "0:7:debug 6:5:notice" {
		t.Fatalf("live %s", got)
	}

	// Replay honours the stream's level.
	ch4, cancel4 := h.openStream(admin, "?loglevel=4")
	defer cancel4()
	if got := texts(replay(t, ch4)); got != "3:4:c 5:2:e" {
		t.Fatalf("level 4 replayed %s", got)
	}
	// backlog=0 skips the replay but still sends the event.
	ch0, cancel0 := h.openStream(admin, "?loglevel=7&backlog=0")
	defer cancel0()
	if got := replay(t, ch0); len(got) != 0 {
		t.Fatalf("backlog=0 replayed %s", texts(got))
	}
	f.publish(logMsg("1", "alert"))
	if l := decodeLog(t, next(t, ch0)); l.Seq != 7 || l.Text != "alert" {
		t.Fatalf("live after backlog=0: %+v", l)
	}
	ch1, cancel1 := h.openStream(admin, "?loglevel=7&backlog=1")
	defer cancel1()
	if got := texts(replay(t, ch1)); got != "4:6:d 5:2:e 6:5:notice 7:1:alert" {
		t.Fatalf("backlog=1 replayed %s", got)
	}

	for _, q := range []string{"?loglevel=7&backlog=2", "?backlog=x", "?loglevel=7&backlog=true"} {
		expect(t, h.do("GET", "/api/instances/"+h.id+"/stream"+q, admin, ""), 400, `{"error":"backlog must be 0 or 1"}`)
	}

	// Streams without logs get no replay and no backlog event.
	viewer := h.login("viewer", viewerPW)
	for _, tok := range []string{viewer, admin} {
		query := "?loglevel=7"
		if tok == admin {
			query = ""
		}
		chv, cancelv := h.openStream(tok, query)
		next(t, chv)
		f.publish(heartbeat)
		if ev := next(t, chv); ev.name != "heartbeat" {
			t.Fatalf("stream without logs got %+v", ev)
		}
		cancelv()
	}
}

func TestBacklogLevelFloor(t *testing.T) {
	h := newHarness(t, withBacklog(10, 6))
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	levels := func(f *fakeSwitch) []int { _, l, _, _ := f.snapshot(); return l }
	expectLevels := func(f *fakeSwitch, want ...int) {
		t.Helper()
		waitFor(t, fmt.Sprint("log levels ", want), func() bool { return reflect.DeepEqual(levels(f), want) })
	}
	// Applied when the instance is created, before any stream opens.
	expectLevels(f, 6)

	a, cancelA := h.openStream(admin, "?loglevel=7")
	replay(t, a)
	expectLevels(f, 6, 7)
	b, cancelB := h.openStream(admin, "?loglevel=3")
	replay(t, b)
	cancelA()
	expectLevels(f, 6, 7, 6) // not 3: the floor holds
	cancelB()
	time.Sleep(50 * time.Millisecond)
	expectLevels(f, 6, 7, 6)

	// The level-3 stream still only gets lines up to 3.
	c, cancelC := h.openStream(admin, "?loglevel=3")
	defer cancelC()
	replay(t, c)
	f.publish(logMsg("6", "info"))
	f.publish(logMsg("3", "error"))
	if l := decodeLog(t, next(t, c)); l.Text != "error" {
		t.Fatalf("level 3 stream got %+v", l)
	}

	// Update and create apply the floor to the new switch at once.
	expect(t, h.do("PUT", "/api/instances/"+h.id, admin, `{"name":"fs01","host":"10.0.0.6","port":8021,"password":"`+eslPW+`"}`), 200, "")
	expectLevels(h.fake(1), 6)
	expect(t, h.do("POST", "/api/instances", admin, `{"name":"b","host":"h","port":1,"password":"p"}`), 201, "")
	expectLevels(h.fake(2), 6)
}

func TestBacklogDisabled(t *testing.T) {
	h := newHarness(t, withBacklog(0, 6))
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	f.publish(logMsg("3", "early"))
	time.Sleep(20 * time.Millisecond)
	ch, cancel := h.openStream(admin, "?loglevel=7")
	defer cancel()
	if got := replay(t, ch); len(got) != 0 {
		t.Fatalf("replayed %s", texts(got))
	}
	f.publish(logMsg("3", "live"))
	if l := decodeLog(t, next(t, ch)); l.Seq != 0 || l.Text != "live" {
		t.Fatalf("live %+v", l)
	}
	if _, levels, _, _ := f.snapshot(); !reflect.DeepEqual(levels, []int{7}) {
		t.Fatalf("levels %v", levels)
	}
}

func TestBacklogConfigValidation(t *testing.T) {
	for _, c := range [][2]int{{-1, 6}, {10, -1}, {10, 8}} {
		st := newHarness(t).st
		_, err := New(Config{Store: st, Dial: func(string, string) Switch { return newFake("", "") }, Static: fstest.MapFS{}, LogBacklog: c[0], LogBacklogLevel: c[1]})
		if err == nil {
			t.Errorf("backlog %d level %d accepted", c[0], c[1])
		}
	}
}

// TestBacklogHandOver publishes without pause while streams open: every
// stream must see a gapless, duplicate-free run of sequence numbers from its
// first replayed line to the last line published.
func TestBacklogHandOver(t *testing.T) {
	h := newHarness(t, withBacklog(50, 6))
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	inst := h.instance(h.id)

	const total = 3000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 1; n <= total; n++ {
			f.publish(logMsg("6", fmt.Sprint("line ", n)))
			if n%20 == 0 {
				time.Sleep(time.Millisecond) // stay within the subscriber buffers
			}
		}
	}()

	var wg sync.WaitGroup
	for s := 0; s < 8; s++ {
		time.Sleep(time.Duration(s) * 3 * time.Millisecond)
		ch, cancel := h.openStream(admin, "?loglevel=7")
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			if ev := <-ch; ev.name != "state" {
				t.Errorf("first %+v", ev)
				return
			}
			var prev uint64
			replayed := -1
			count := 0
			for {
				var ev sseEvent
				select {
				case ev = <-ch:
				case <-time.After(10 * time.Second):
					t.Errorf("stream stalled after seq %d", prev)
					return
				}
				switch ev.name {
				case "backlog":
					replayed = count
					continue
				case "log":
				default:
					continue
				}
				var l logEvent
				json.Unmarshal([]byte(ev.data), &l)
				if prev != 0 && l.Seq != prev+1 {
					t.Errorf("seq %d after %d (replayed %d)", l.Seq, prev, replayed)
					return
				}
				prev = l.Seq
				count++
				if l.Text == "end" {
					if want := inst.lastSeq(); prev != want {
						t.Errorf("ended at seq %d, last stored %d", prev, want)
					}
					return
				}
			}
		}()
	}
	<-done
	time.Sleep(50 * time.Millisecond) // let the collector drain
	f.publish(logMsg("6", "end"))
	wg.Wait()
	if n := inst.lastSeq(); n < total/2 {
		t.Fatalf("collector kept only %d of %d lines", n, total)
	}
}
