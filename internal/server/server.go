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
}

type instance struct {
	sw   Switch
	done chan struct{} // closed when the instance is deleted or replaced, or the server closes

	logMu   sync.Mutex
	levels  map[int]int // log level -> open streams asking for it
	applied int         // level last passed to SetLogLevel
}

func newInstance(sw Switch) *instance {
	return &instance{sw: sw, done: make(chan struct{}), levels: map[int]int{}, applied: -1}
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
		ctx, cancel := context.WithTimeout(r.Context(), eslTimeout)
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
