package demo

import (
	"container/heap"
	"context"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
)

// Switch is one simulated FreeSWITCH behind the interface of the ESL client
// (server.Switch). Its methods are safe for concurrent use.
type Switch struct {
	sp *spec

	clock      func() time.Time // the wall clock of a live switch; nil when Advance drives it
	stop, done chan struct{}    // of the run loop of a live switch
	closeOnce  sync.Once

	mu       sync.Mutex
	state    esl.State
	closed   bool
	subs     map[subscriber]struct{}
	eslLevel int // the level SetLogLevel asked for; -1 for no log lines

	// The simulation; everything below is guarded by mu.
	seed     int64 // of rng; History draws from a source of its own
	rng      *rand.Rand
	sipRng   *rand.Rand // of the live SIP events (sipevents.go)
	t        time.Time  // simulated now
	boot     time.Time
	queue    eventQueue
	seq      uint64 // orders events scheduled for the same time
	eventSeq uint64 // Event-Sequence of the published events
	changes  map[string]*change
	conf     *configuration
	coreUUID string

	profiles []*profile
	gateways []*gateway
	regs     []*registration
	modules  []*module
	vars     []string          // names of the global variables in the order they were set
	values   map[string]string // global variables
	channels map[string]*channel
	sessionN int // number of the last session created

	coreLevel   int // fsctl loglevel
	maxSessions int
	sps         int
	paused      bool
	idle        float64 // idle CPU, percent

	peak, peakSince int
	peaks           [15]int // the peaks of the last 15 heartbeat intervals: 5 minutes
	rateSec         int64   // the second rateNow counts
	rateNow         int
	rateLast        int // sessions created in the second before rateSec
	ratePeak        int
	rateSince       int
	ratePeaks       [15]int
	beats           int
}

type subscriber struct{ events, logs chan esl.Message }

// event is something the simulation does at a time.
type event struct {
	at    time.Time
	seq   uint64
	fn    func()
	index int // in the queue; -1 once it has run or been cancelled
}

type eventQueue []*event

func (q eventQueue) Len() int { return len(q) }
func (q eventQueue) Less(i, j int) bool {
	if !q[i].at.Equal(q[j].at) {
		return q[i].at.Before(q[j].at)
	}
	return q[i].seq < q[j].seq
}
func (q eventQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *eventQueue) Push(x any) {
	e := x.(*event)
	e.index = len(*q)
	*q = append(*q, e)
}
func (q *eventQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	e.index = -1
	return e
}

// change is a visitor's change, undone at at.
type change struct {
	at   time.Time
	undo func()
}

func newSwitch(addr string, seed int64, start time.Time) *Switch {
	s := &Switch{
		sp:       specAt(addr),
		subs:     map[subscriber]struct{}{},
		eslLevel: -1,
		t:        start,
		state:    esl.State{Since: start},
	}
	if s.sp == nil || s.sp.down != "" {
		reason := "connection refused"
		if s.sp != nil {
			reason = s.sp.down
		}
		s.state.LastError = "dial tcp " + addr + ": connect: " + reason
		return s
	}
	s.state.Connected = true
	s.seed, s.rng = seed, rand.New(rand.NewSource(seed))
	s.setup()
	return s
}

// run moves a live switch along the wall clock.
func (s *Switch) run() {
	defer close(s.done)
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			s.mu.Lock()
			s.step(s.clock())
			s.mu.Unlock()
		}
	}
}

// Advance moves the clock of a switch from New on by d, carrying out
// everything that happens meanwhile.
func (s *Switch) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rng != nil && !s.closed {
		s.step(s.t.Add(d))
	}
}

// step carries out the events due up to now, then undoes the visitor
// changes that are due, in a fixed order.
func (s *Switch) step(now time.Time) {
	for len(s.queue) > 0 && !s.queue[0].at.After(now) {
		e := heap.Pop(&s.queue).(*event)
		if e.at.After(s.t) {
			s.t = e.at
		}
		e.fn()
	}
	if now.After(s.t) {
		s.t = now
	}
	var due []string
	for key, c := range s.changes {
		if !c.at.After(s.t) {
			due = append(due, key)
		}
	}
	sort.Strings(due)
	for _, key := range due {
		c := s.changes[key]
		delete(s.changes, key)
		c.undo()
	}
}

// after schedules fn d from now.
func (s *Switch) after(d time.Duration, fn func()) *event {
	s.seq++
	e := &event{at: s.t.Add(d), seq: s.seq, fn: fn}
	heap.Push(&s.queue, e)
	return e
}

// cancel takes e out of the queue if it has not run yet, so whatever a
// visitor ends early leaves nothing behind.
func (s *Switch) cancel(e *event) {
	if e != nil && e.index >= 0 {
		heap.Remove(&s.queue, e.index)
	}
}

// changed records a visitor's change of key before it is made: undo restores
// the state before it. A later change of the same key keeps the first undo
// and moves the time it runs.
func (s *Switch) changed(key string, undo func()) {
	if c, ok := s.changes[key]; ok {
		c.at = s.t.Add(healAfter)
		return
	}
	s.changes[key] = &change{at: s.t.Add(healAfter), undo: undo}
}

func (s *Switch) State() esl.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// API answers command from the simulation. Errors are those of the ESL
// client: a line break, a context that has ended, no connection.
func (s *Switch) API(ctx context.Context, command string) (string, error) {
	if strings.ContainsAny(command, "\r\n") {
		return "", esl.ErrInvalidCommand
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.state.Connected {
		return "", esl.ErrNotConnected
	}
	if s.clock != nil {
		s.step(s.clock())
	}
	return s.command(command), nil
}

// Background answers like API: no simulated command is slow.
func (s *Switch) Background(ctx context.Context, command string) (string, error) {
	return s.API(ctx, command)
}

// SetLogLevel sets the highest level of the log lines delivered: -1 for
// none, 0 to 7.
func (s *Switch) SetLogLevel(level int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eslLevel = max(-1, min(level, 7))
}

// Subscribe works as the ESL client's: a subscriber that does not keep up
// loses messages, and cancel closes both channels.
func (s *Switch) Subscribe(buffer int) (events, logs <-chan esl.Message, cancel func()) {
	sub := subscriber{events: make(chan esl.Message, max(buffer, 0)), logs: make(chan esl.Message, max(buffer, 0))}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		close(sub.events)
		close(sub.logs)
		return sub.events, sub.logs, func() {}
	}
	s.subs[sub] = struct{}{}
	return sub.events, sub.logs, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.events)
			close(sub.logs)
		}
	}
}

// Close stops the simulation and closes the channels of every subscriber.
func (s *Switch) Close() {
	s.closeOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		if s.state.Connected {
			s.state = esl.State{Since: time.Now(), LastError: "esl: client closed"}
		}
		for sub := range s.subs {
			delete(s.subs, sub)
			close(sub.events)
			close(sub.logs)
		}
		s.queue, s.channels, s.changes = nil, nil, nil
	})
}

// publish delivers m to every subscriber without blocking, as the ESL
// client does.
func (s *Switch) publish(m esl.Message) {
	for sub := range s.subs {
		ch := sub.events
		if m.Kind == esl.KindLog {
			ch = sub.logs
		}
		select {
		case ch <- m:
		default:
		}
	}
}
