// Package server is the fsdeck HTTP API, authentication, event stream and
// static file server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"fsdeck/internal/esl"
	"fsdeck/internal/fsapi"
	"fsdeck/internal/store"
)

// Switch is one ESL connection to a FreeSWITCH. *esl.Client satisfies it.
type Switch interface {
	State() esl.State
	API(ctx context.Context, command string) (string, error)
	SetLogLevel(level int)
	Subscribe(buffer int) (<-chan esl.Message, func())
	Close()
}

type Config struct {
	AdminPassword  string // required
	ViewerPassword string // empty disables the viewer account
	Store          *store.Store
	Dial           func(addr, password string) Switch // returns a started client
	Static         fs.FS
}

const (
	eslTimeout     = 15 * time.Second
	consoleTimeout = 60 * time.Second
	maxBody        = 1 << 20
)

type Server struct {
	cfg          Config
	mux          *http.ServeMux
	loginDelay   time.Duration
	pingInterval time.Duration

	mu    sync.Mutex // guards insts and serializes instance changes
	insts map[string]*instance

	sessMu   sync.Mutex
	sessions map[string]session

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

	logMu   sync.Mutex
	levels  map[int]int // log level -> open streams asking for it
	applied int         // level last passed to SetLogLevel

	histMu    sync.Mutex
	history   [historySize]sample // ring; the oldest is at histNext once full
	histNext  int
	histLen   int
	collected chan struct{} // closed when the history collector has returned
}

// newInstance starts the history collector of sw.
func newInstance(sw Switch) *instance {
	i := &instance{sw: sw, done: make(chan struct{}), levels: map[int]int{}, applied: -1, collected: make(chan struct{})}
	msgs, cancel := sw.Subscribe(64)
	go i.collect(msgs, cancel)
	return i
}

// New dials every stored instance.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.AdminPassword == "":
		return nil, errors.New("server: admin password is required")
	case cfg.Store == nil || cfg.Dial == nil || cfg.Static == nil:
		return nil, errors.New("server: Store, Dial and Static are required")
	}
	s := &Server{
		cfg:          cfg,
		loginDelay:   500 * time.Millisecond,
		pingInterval: 20 * time.Second,
		insts:        map[string]*instance{},
		sessions:     map[string]session{},

		loginFails:    map[loginKey]*loginRecord{},
		loginMaxFails: 5,
		loginWindow:   5 * time.Minute,
		loginBlock:    time.Minute,
		loginMaxKeys:  10000,
	}
	for _, in := range cfg.Store.List() {
		s.insts[in.ID] = newInstance(s.dial(in))
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

func (s *Server) dial(in store.Instance) Switch {
	return s.cfg.Dial(net.JoinHostPort(in.Host, strconv.Itoa(in.Port)), in.Password)
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.auth(false, s.handleLogout))
	m.HandleFunc("GET /api/me", s.auth(false, s.handleMe))

	m.HandleFunc("GET /api/instances", s.auth(false, s.handleListInstances))
	m.HandleFunc("POST /api/instances", s.auth(true, s.handleCreateInstance))
	m.HandleFunc("PUT /api/instances/{id}", s.auth(true, s.handleUpdateInstance))
	m.HandleFunc("DELETE /api/instances/{id}", s.auth(true, s.handleDeleteInstance))

	m.HandleFunc("GET /api/instances/{id}/status", s.auth(false, s.handleStatus))
	m.HandleFunc("GET /api/instances/{id}/sofia", rowsRoute(s, fsapi.SofiaStatus))
	m.HandleFunc("GET /api/instances/{id}/gateways", rowsRoute(s, fsapi.SofiaGateways))
	for _, kind := range []string{"registrations", "channels", "calls", "modules"} {
		m.HandleFunc("GET /api/instances/{id}/"+kind, rowsRoute(s, func(ctx context.Context, c fsapi.Commander) ([]map[string]any, error) {
			return fsapi.Rows(ctx, c, kind)
		}))
	}
	m.HandleFunc("GET /api/instances/{id}/vars", rowsRoute(s, fsapi.GlobalVars))
	m.HandleFunc("GET /api/instances/{id}/config", rowsRoute(s, fsapi.ConfigList))
	m.HandleFunc("GET /api/instances/{id}/config/{name}", s.auth(false, s.handleConfig))
	m.HandleFunc("GET /api/instances/{id}/dialplan", rowsRoute(s, fsapi.DialplanContexts))
	m.HandleFunc("GET /api/instances/{id}/dialplan/{name}", xmlRoute(s, fsapi.DialplanContext))
	m.HandleFunc("GET /api/instances/{id}/directory", rowsRoute(s, fsapi.DirectoryDomains))
	m.HandleFunc("GET /api/instances/{id}/directory/{name}", xmlRoute(s, fsapi.DirectoryDomain))
	m.HandleFunc("GET /api/instances/{id}/history", s.auth(false, s.handleHistory))
	m.HandleFunc("DELETE /api/instances/{id}/channels/{uuid}", s.auth(true, s.handleHangup))
	m.HandleFunc("POST /api/instances/{id}/actions", s.auth(true, s.handleAction))
	m.HandleFunc("POST /api/instances/{id}/console", s.auth(true, s.handleConsole))
	m.HandleFunc("GET /api/instances/{id}/stream", s.auth(false, s.handleStream))

	m.HandleFunc("/api/", s.auth(false, func(w http.ResponseWriter, _ *http.Request, _ user) {
		writeError(w, http.StatusNotFound, "not found")
	}))
	m.Handle("/", http.FileServerFS(s.cfg.Static))
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
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
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
func rowsRoute[T any](s *Server, fetch func(context.Context, fsapi.Commander) ([]T, error)) http.HandlerFunc {
	return s.auth(false, func(w http.ResponseWriter, r *http.Request, _ user) {
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
	})
}

// xmlRoute serves one named XML entry as {"name": ..., "xml": ...}.
func xmlRoute(s *Server, fetch func(context.Context, fsapi.Commander, string) (string, error)) http.HandlerFunc {
	return s.auth(false, func(w http.ResponseWriter, r *http.Request, _ user) {
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
	})
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
