// Package server is the fsconsole HTTP API, authentication, event stream and
// static file server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juki/freeswitch-console/internal/audit"
	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/fsapi"
	"github.com/juki/freeswitch-console/internal/store"
)

// Switch is one ESL connection to a FreeSWITCH. *esl.Client satisfies it.
type Switch interface {
	State() esl.State
	API(ctx context.Context, command string) (string, error)
	// Background runs a command that can be slow in a thread of its own on
	// the switch, so it does not hold the connection.
	Background(ctx context.Context, command string) (string, error)
	SetLogLevel(level int)
	Subscribe(buffer int) (events, logs <-chan esl.Message, cancel func())
	Close()
}

type Config struct {
	AdminPassword  string // FSCONSOLE_ADMIN_PASSWORD: the managed account admin; empty for none
	ViewerPassword string // FSCONSOLE_VIEWER_PASSWORD: the managed account viewer; empty for none
	Store          *store.Store
	Dial           func(addr, password string) Switch // returns a started client
	Static         fs.FS
	Version        string // reported by GET /api/version; empty is "dev"

	LogBacklog      int // log lines kept per instance; 0 disables the backlog
	LogBacklogLevel int // highest log level kept, 0..7

	RecentCalls int // hang-up records kept per instance; 0 turns them off

	Audit *audit.Log // nil disables the audit log

	BackupDir string // config file backups of local-mode instances, one directory per instance
}

const (
	eslTimeout     = 15 * time.Second
	consoleTimeout = 60 * time.Second
	maxBody        = 1 << 20
	maxFileBody    = 8 << 20
)

type Server struct {
	cfg          Config
	mux          *http.ServeMux
	loginDelay   time.Duration
	pingInterval time.Duration
	// streamCheck is how often an open stream checks its session again.
	streamCheck time.Duration

	// Open event streams, at most maxSessionStreams per session token and
	// maxStreams in all.
	streamsMu         sync.Mutex
	sessionStreams    map[string]int
	openStreams       int
	maxSessionStreams int
	maxStreams        int

	mu    sync.Mutex // guards insts and serializes instance changes
	insts map[string]*instance

	sessMu   sync.Mutex
	sessions map[string]session

	managed   []account  // from the environment; they hide file users of the same name
	dummyHash string     // verified for unknown usernames so they cost the same
	usersMu   sync.Mutex // serializes account changes, so the admin guards hold

	verifySlots chan struct{} // one token per password hash or check in progress
	verifyWait  time.Duration // how long a request waits for a slot before 503

	// verifyIP counts the sign-in password checks in progress per client
	// address, at most verifyPerIP each, so one address cannot hold every
	// slot. An address is removed at zero.
	verifyIPMu  sync.Mutex
	verifyIP    map[string]int
	verifyPerIP int

	routeCaps map[string]capability // pattern -> capability, for the route table test

	// loginMaxFails failures within loginWindow block a key for loginBlock;
	// the table holds at most loginMaxKeys keys.
	loginMu       sync.Mutex
	loginFails    map[loginKey]*loginRecord
	loginMaxFails int
	loginWindow   time.Duration
	loginBlock    time.Duration
	loginMaxKeys  int
}

type instance struct {
	sw   Switch
	done chan struct{} // closed when the instance is deleted or replaced, or the server closes

	// logMu guards the log-level bookkeeping, the backlog, the open streams
	// and the channels that hung up recently. The collector stores and
	// forwards each message while holding it, so a stream that copies the
	// backlog and registers in one critical section gets every later line
	// exactly once; so does a hang-up record.
	logMu       sync.Mutex
	levels      map[int]int // log level -> open streams asking for it
	applied     int         // level last passed to SetLogLevel
	floor       int         // the level never goes below this: the backlog level, or -1 without a backlog
	streams     map[*stream]struct{}
	backlog     []logEvent // ring; the oldest is at backlogNext once full
	backlogSize int        // 0 disables the backlog
	backlogNext int
	seq         uint64                  // sequence number of the last stored line
	recent      map[string]recentHangup // channel uuid -> its record, while late lines are attached
	lateLog     time.Duration           // how long after the hang-up a channel's lines are attached

	histMu    sync.Mutex
	history   [historySize]sample // ring; the oldest is at histNext once full
	histNext  int
	histLen   int
	collected chan struct{} // closed when the collector has returned

	// callsMu guards the records and the log of each. It is taken after
	// logMu, never before.
	callsMu   sync.Mutex
	calls     []*hangup // ring; the oldest is at callsNext once full
	callsSize int       // 0 turns the records off
	callsNext int
}

// newInstance starts the collector of sw. With a backlog the ESL log level is
// raised to backlogLevel at once, so lines are kept before any stream opens.
// recentCalls is the number of hang-up records kept.
func newInstance(sw Switch, backlog, backlogLevel, recentCalls int) *instance {
	i := &instance{
		sw:          sw,
		done:        make(chan struct{}),
		levels:      map[int]int{},
		applied:     -1,
		floor:       -1,
		streams:     map[*stream]struct{}{},
		backlogSize: backlog,
		collected:   make(chan struct{}),
		callsSize:   recentCalls,
		recent:      map[string]recentHangup{},
		lateLog:     hangupLateLog,
	}
	events, logs, cancel := sw.Subscribe(1024)
	if backlog > 0 {
		i.floor, i.applied = backlogLevel, backlogLevel
		sw.SetLogLevel(backlogLevel)
	}
	go i.collect(events, logs, cancel)
	return i
}

// New dials every stored instance.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Store == nil || cfg.Dial == nil || cfg.Static == nil:
		return nil, errors.New("server: Store, Dial and Static are required")
	case cfg.LogBacklog < 0:
		return nil, errors.New("server: the log backlog must not be negative")
	case cfg.LogBacklog > 0 && (cfg.LogBacklogLevel < 0 || cfg.LogBacklogLevel > 7):
		return nil, errors.New("server: the log backlog level must be 0 to 7")
	case cfg.RecentCalls < 0:
		return nil, errors.New("server: the number of recent calls must not be negative")
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &Server{
		cfg:          cfg,
		loginDelay:   500 * time.Millisecond,
		pingInterval: 20 * time.Second,
		streamCheck:  5 * time.Second,
		insts:        map[string]*instance{},
		sessions:     map[string]session{},

		sessionStreams:    map[string]int{},
		maxSessionStreams: 8,
		maxStreams:        512,

		loginFails:    map[loginKey]*loginRecord{},
		loginMaxFails: 5,
		loginWindow:   5 * time.Minute,
		loginBlock:    time.Minute,
		loginMaxKeys:  10000,
		routeCaps:     map[string]capability{},
		verifySlots:   make(chan struct{}, 4),
		verifyWait:    2 * time.Second,
		verifyIP:      map[string]int{},
		verifyPerIP:   2,
	}
	for _, m := range []struct{ id, name, role, password string }{
		{managedAdminID, "admin", store.RoleAdmin, cfg.AdminPassword},
		{managedViewerID, "viewer", store.RoleViewer, cfg.ViewerPassword},
	} {
		if m.password == "" {
			continue
		}
		hash, err := hashPassword(m.password)
		if err != nil {
			return nil, err
		}
		s.managed = append(s.managed, account{User: store.User{ID: m.id, Username: m.name, Role: m.role, PasswordHash: hash}, Managed: true})
	}
	dummy, err := randomPassword()
	if err == nil {
		s.dummyHash, err = hashPassword(dummy)
	}
	if err != nil {
		return nil, err
	}
	for _, in := range cfg.Store.List() {
		s.insts[in.ID] = s.newInstance(in)
	}
	s.routes()
	return s, nil
}

// Close closes every Switch and ends every open stream.
func (s *Server) Close() {
	s.mu.Lock()
	insts := s.insts
	s.insts = map[string]*instance{}
	s.mu.Unlock()
	for _, inst := range insts {
		inst.close()
	}
}

func (i *instance) close() {
	close(i.done)
	i.sw.Close()
	<-i.collected
}

// eslContext bounds one ESL call. It deliberately does not follow the
// request's cancellation: the connection is shared, and the ESL client drops
// it when a pending reply is abandoned, so a browser that aborts a request
// would disconnect the instance for every other viewer.
func eslContext(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), d)
}

// newInstance dials in and starts its collector. The handlers reach the
// switch only through the instance, so every API call they make passes the
// limiter.
func (s *Server) newInstance(in store.Instance) *instance {
	sw := s.cfg.Dial(net.JoinHostPort(in.Host, strconv.Itoa(in.Port)), in.Password)
	return newInstance(&limitedSwitch{Switch: sw, slots: make(chan struct{}, apiSlots), bgSlots: make(chan struct{}, backgroundSlots), wait: apiWait}, s.cfg.LogBacklog, s.cfg.LogBacklogLevel, s.cfg.RecentCalls)
}

// The API calls of one instance share one ESL connection, and a call that
// passes its deadline drops that connection for everyone. So at most
// apiSlots calls from handlers are in flight per instance. Background
// commands do not hold the connection but each runs in a thread of the
// switch; at most backgroundSlots of them are in flight per instance. A call
// waits at most apiWait for a slot.
const (
	apiSlots        = 16
	backgroundSlots = 8
	apiWait         = 5 * time.Second
)

var errInstanceBusy = errors.New("too many requests to this instance at once; try again in a moment")

// limitedSwitch bounds the API and Background calls in flight. The ESL
// client's own commands (probe, log level) and the collector do not go
// through them.
type limitedSwitch struct {
	Switch
	slots   chan struct{} // API calls
	bgSlots chan struct{} // Background calls
	wait    time.Duration
}

func (l *limitedSwitch) API(ctx context.Context, command string) (string, error) {
	if err := l.acquire(ctx, l.slots); err != nil {
		return "", err
	}
	defer func() { <-l.slots }()
	return l.Switch.API(ctx, command)
}

func (l *limitedSwitch) Background(ctx context.Context, command string) (string, error) {
	if err := l.acquire(ctx, l.bgSlots); err != nil {
		return "", err
	}
	defer func() { <-l.bgSlots }()
	return l.Switch.Background(ctx, command)
}

// acquire takes a slot of slots, waiting at most l.wait.
func (l *limitedSwitch) acquire(ctx context.Context, slots chan struct{}) error {
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case slots <- struct{}{}:
		return nil
	case <-t.C:
		return errInstanceBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errStillRunning = errors.New("the command is still running on FreeSWITCH")

// inBackground is the fsapi.Commander of a route that runs its command with
// Background under the deadline d. When d passes, the command keeps running
// on the switch, and the error says so.
type inBackground struct {
	sw Switch
	d  time.Duration
}

func (b inBackground) API(ctx context.Context, command string) (string, error) {
	out, err := b.sw.Background(ctx, command)
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w after %ds; its result will not be shown", errStillRunning, int(b.d/time.Second))
	}
	return out, err
}

func (s *Server) routes() {
	m := http.NewServeMux()
	s.handlePublic(m, "GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.handlePublic(m, "POST /api/login", s.handleLogin)
	s.handle(m, "POST /api/logout", capRead, "", s.handleLogout)
	s.handle(m, "GET /api/me", capRead, "", s.handleMe)
	s.handle(m, "POST /api/me/password", capRead, "", s.handleChangePassword)
	s.handle(m, "GET /api/version", capRead, "", func(w http.ResponseWriter, _ *http.Request, _ user) {
		writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
	})

	s.handle(m, "GET /api/users", capUsers, "user.list", s.handleListUsers)
	s.handle(m, "POST /api/users", capUsers, "user.create", s.handleCreateUser)
	s.handle(m, "PUT /api/users/{id}", capUsers, "user.update", s.handleUpdateUser)
	s.handle(m, "DELETE /api/users/{id}", capUsers, "user.delete", s.handleDeleteUser)
	s.handle(m, "GET /api/audit", capUsers, "audit.read", s.handleAudit)
	s.registerFiles(m)

	s.handle(m, "GET /api/instances", capRead, "", s.handleListInstances)
	s.handle(m, "POST /api/instances", capInstances, "instance.create", s.handleCreateInstance)
	s.handle(m, "PUT /api/instances/{id}", capInstances, "instance.update", s.handleUpdateInstance)
	s.handle(m, "DELETE /api/instances/{id}", capInstances, "instance.delete", s.handleDeleteInstance)

	s.handle(m, "GET /api/instances/{id}/status", capRead, "", s.handleStatus)
	s.handle(m, "GET /api/instances/{id}/sofia", capRead, "", rowsRoute(s, fsapi.SofiaStatus))
	s.handle(m, "GET /api/instances/{id}/gateways", capRead, "", rowsRoute(s, fsapi.SofiaGateways))
	for _, kind := range []string{"registrations", "channels", "calls", "modules"} {
		s.handle(m, "GET /api/instances/{id}/"+kind, capRead, "", rowsRoute(s, func(ctx context.Context, c fsapi.Commander) ([]map[string]any, error) {
			return fsapi.Rows(ctx, c, kind)
		}))
	}
	s.handle(m, "GET /api/instances/{id}/vars", capRead, "", rowsRoute(s, fsapi.GlobalVars))
	s.handle(m, "GET /api/instances/{id}/config", capRead, "", rowsRoute(s, fsapi.ConfigList))
	s.handle(m, "GET /api/instances/{id}/config/{name}", capRead, "", s.handleConfig)
	s.handle(m, "GET /api/instances/{id}/dialplan", capRead, "", rowsRoute(s, fsapi.DialplanContexts))
	s.handle(m, "GET /api/instances/{id}/dialplan/{name}", capRead, "", xmlRoute(s, fsapi.DialplanContext))
	s.handle(m, "GET /api/instances/{id}/directory", capRead, "", rowsRoute(s, fsapi.DirectoryDomains))
	s.handle(m, "GET /api/instances/{id}/directory/{name}", capRead, "", xmlRoute(s, fsapi.DirectoryDomain))
	s.handle(m, "GET /api/instances/{id}/history", capRead, "", s.handleHistory)
	// The call's log lines need capLogs, checked in the handler.
	s.handle(m, "GET /api/instances/{id}/hangups", capRead, "", s.handleHangups)
	s.handle(m, "GET /api/instances/{id}/hangups/{uuid}", capRead, "", s.handleHangupDetail)
	// The stream is readable by every role; log lines need capLogs, checked
	// in the handler.
	s.handle(m, "GET /api/instances/{id}/stream", capRead, "", s.handleStream)
	s.handle(m, "DELETE /api/instances/{id}/channels/{uuid}", capOperate, "channel.hangup", s.handleHangup)
	s.handle(m, "POST /api/instances/{id}/actions", capOperate, "action.*", s.handleAction)
	s.handle(m, "POST /api/instances/{id}/console", capConsole, "console.run", s.handleConsole)
	s.handle(m, "POST /api/instances/{id}/console/complete", capConsole, "console.complete", s.handleComplete)
	s.handle(m, "GET /api/instances/{id}/console/commands", capConsole, "console.commands", s.handleCommands)

	s.handle(m, "/api/", capRead, "", func(w http.ResponseWriter, _ *http.Request, _ user) {
		writeError(w, http.StatusNotFound, "not found")
	})
	s.handlePublic(m, "/", http.FileServerFS(s.cfg.Static).ServeHTTP)
	s.mux = m
}

// Handler returns the HTTP handler: security headers on every response, and
// for the API no caching, a body size limit and a JSON content type.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			limit := int64(maxBody)
			if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/files/content") {
				limit = maxFileBody // a config file of up to 1 MiB, JSON-escaped
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodDelete:
				// ContentLength is -1 for a chunked body of unknown length.
				if r.ContentLength != 0 {
					mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
					if mt != "application/json" {
						writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
						return
					}
				}
			}
		} else {
			// The embedded files carry no validators and keep their names
			// across versions; without this a browser can run a stale
			// frontend against a newer server.
			h.Set("Cache-Control", "no-cache")
		}
		s.mux.ServeHTTP(w, r)
	})
}

// rowsRoute serves a read-only query as {"rows": [...]}.
func rowsRoute[T any](s *Server, fetch func(context.Context, fsapi.Commander) ([]T, error)) func(http.ResponseWriter, *http.Request, user) {
	return func(w http.ResponseWriter, r *http.Request, _ user) {
		inst := s.connected(w, r)
		if inst == nil {
			return
		}
		ctx, cancel := eslContext(r, eslTimeout)
		defer cancel()
		rows, err := fetch(ctx, inst.sw)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if rows == nil {
			rows = []T{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
	}
}

// xmlRoute serves one named XML entry as {"name": ..., "xml": ...}.
func xmlRoute(s *Server, fetch func(context.Context, fsapi.Commander, string) (string, error)) func(http.ResponseWriter, *http.Request, user) {
	return func(w http.ResponseWriter, r *http.Request, _ user) {
		inst := s.connected(w, r)
		if inst == nil {
			return
		}
		ctx, cancel := eslContext(r, eslTimeout)
		defer cancel()
		name := r.PathValue("name")
		doc, err := fetch(ctx, inst.sw, name)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": name, "xml": doc})
	}
}

// lookup writes 404 and returns nil for an unknown instance.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) *instance {
	s.mu.Lock()
	inst := s.insts[r.PathValue("id")]
	s.mu.Unlock()
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
	}
	return inst
}

// ready writes 503 and returns false when the instance is not connected.
func ready(w http.ResponseWriter, inst *instance) bool {
	if !inst.sw.State().Connected {
		writeError(w, http.StatusServiceUnavailable, "instance not connected")
		return false
	}
	return true
}

// connected combines lookup and ready.
func (s *Server) connected(w http.ResponseWriter, r *http.Request) *instance {
	inst := s.lookup(w, r)
	if inst == nil || !ready(w, inst) {
		return nil
	}
	return inst
}

// writeAPIError maps fsapi errors; anything else is a transport error.
func writeAPIError(w http.ResponseWriter, err error) {
	var ce *fsapi.CommandError
	code := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, fsapi.ErrInvalidArgument):
		code = http.StatusBadRequest
	case errors.Is(err, fsapi.ErrNotFound):
		code = http.StatusNotFound
	case errors.As(err, &ce), errors.Is(err, fsapi.ErrBadReply):
		code = http.StatusBadGateway
	case errors.Is(err, errStillRunning):
		code = http.StatusGatewayTimeout
	}
	writeError(w, code, err.Error())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decodeJSON decodes exactly one JSON value with no unknown fields; on
// failure it writes the error response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("unexpected data after JSON value")
		}
	}
	var mbe *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	case err == io.EOF:
		writeError(w, http.StatusBadRequest, "request body required")
	default:
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
	}
	return false
}
