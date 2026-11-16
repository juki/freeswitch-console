// Package store keeps the FreeSWITCH instances and the console accounts in a
// JSON file.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Instance is one FreeSWITCH reachable over inbound ESL.
type Instance struct {
	ID       string `json:"id"` // 16 hex chars, generated
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"` // ESL password; encrypted in the file when a secret key is set

	// Config file access: ConfigMode "" (none), "local" (ConfigPath is the
	// conf directory on this host) or "agent" (the fsconsole agent at AgentURL,
	// AgentToken encrypted like Password).
	ConfigMode string `json:"configMode,omitempty"`
	ConfigPath string `json:"configPath,omitempty"`
	AgentURL   string `json:"agentURL,omitempty"`
	AgentToken string `json:"agentToken,omitempty"`
}

// Config access modes.
const (
	ConfigLocal = "local"
	ConfigAgent = "agent"
)

// Roles of a console account.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// User is a console account. The store keeps the password hash as given; the
// server creates and checks it.
type User struct {
	ID           string    `json:"id"` // 16 hex chars, generated
	Username     string    `json:"username"`
	Role         string    `json:"role"`
	PasswordHash string    `json:"passwordHash"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"createdAt"`
}

var (
	ErrNotFound     = errors.New("instance not found")
	ErrInvalid      = errors.New("invalid instance")
	ErrUserNotFound = errors.New("user not found")
	ErrInvalidUser  = errors.New("invalid user")
	ErrDuplicate    = errors.New("username already exists")

	ErrSecretKeyShort   = errors.New("the secret key (FSCONSOLE_SECRET_KEY) must be at least 16 characters")
	ErrSecretKeyMissing = errors.New("the file holds encrypted ESL passwords but no secret key is set (FSCONSOLE_SECRET_KEY)")
	ErrSecretKeyWrong   = errors.New("cannot decrypt the ESL passwords: the secret key (FSCONSOLE_SECRET_KEY) is wrong")
)

// encPrefix marks an encrypted ESL password: enc:v1:<base64(nonce|ciphertext)>.
const encPrefix = "enc:v1:"

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

// CheckUsername explains why name cannot be a username, or returns nil.
func CheckUsername(name string) error {
	if !usernameRE.MatchString(name) {
		return fmt.Errorf("%w: username must be 2 to 32 characters: lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalidUser)
	}
	return nil
}

// CheckRole explains why role is not a role, or returns nil.
func CheckRole(role string) error {
	if role != RoleAdmin && role != RoleOperator && role != RoleViewer {
		return fmt.Errorf("%w: role must be admin, operator or viewer", ErrInvalidUser)
	}
	return nil
}

// Store is safe for concurrent use.
type Store struct {
	path      string
	aead      cipher.AEAD // nil: ESL passwords are written in clear
	mu        sync.Mutex
	instances []Instance
	users     []User
}

// file is the on-disk shape. Users is omitted while there are none, so a file
// written by an older version keeps its shape.
type file struct {
	Instances []Instance `json:"instances"`
	Users     []User     `json:"users,omitempty"`
}

// Option configures Open.
type Option func(*Store) error

// WithSecretKey encrypts the ESL passwords in the file with AES-256-GCM under
// the SHA-256 of secret. Plain values in an existing file are read as they are
// and encrypted on the next save.
func WithSecretKey(secret string) Option {
	return func(s *Store) error {
		if utf8.RuneCountInString(secret) < 16 {
			return ErrSecretKeyShort
		}
		key := sha256.Sum256([]byte(secret))
		block, err := aes.NewCipher(key[:])
		if err != nil {
			return err
		}
		s.aead, err = cipher.NewGCM(block)
		return err
	}
}

// Open loads the store at path; a missing file is an empty store.
func Open(path string, opts ...Option) (*Store, error) {
	s := &Store{path: path}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	for i := range f.Instances {
		in := &f.Instances[i]
		if in.Password, err = s.reveal(in.Password); err == nil {
			in.AgentToken, err = s.reveal(in.AgentToken)
		}
		if err != nil {
			return nil, fmt.Errorf("store %s: %w", path, err)
		}
	}
	for i, u := range f.Users {
		if err := validateUser(u); err != nil {
			return nil, fmt.Errorf("store %s: user %d: %w", path, i+1, err)
		}
		for _, o := range f.Users[:i] {
			if o.Username == u.Username || o.ID == u.ID {
				return nil, fmt.Errorf("store %s: user %q: %w", path, u.Username, ErrDuplicate)
			}
		}
	}
	s.instances, s.users = f.Instances, f.Users
	return s, nil
}

// reveal returns the clear text of a stored ESL password.
func (s *Store) reveal(v string) (string, error) {
	enc, ok := strings.CutPrefix(v, encPrefix)
	if !ok {
		return v, nil
	}
	if s.aead == nil {
		return "", ErrSecretKeyMissing
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	n := s.aead.NonceSize()
	if err != nil || len(raw) < n {
		return "", errors.New("malformed encrypted ESL password")
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", ErrSecretKeyWrong
	}
	return string(plain), nil
}

func (s *Store) seal(v string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return encPrefix + base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(v), nil)), nil
}

// List returns a copy of the instances in insertion order.
func (s *Store) List() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Instance(nil), s.instances...)
}

func (s *Store) Get(id string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(id); i >= 0 {
		return s.instances[i], true
	}
	return Instance{}, false
}

// Add assigns a new ID, stores the instance and returns it.
func (s *Store) Add(in Instance) (Instance, error) {
	in = normalize(in)
	if err := validate(in); err != nil {
		return Instance{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		id, err := newID()
		if err != nil {
			return Instance{}, err
		}
		if s.index(id) < 0 {
			in.ID = id
			break
		}
	}
	next := append(append([]Instance(nil), s.instances...), in)
	if err := s.save(next, s.users); err != nil {
		return Instance{}, err
	}
	s.instances = next
	return in, nil
}

// Update replaces the instance with the same ID; an empty Password or
// AgentToken keeps the stored one.
func (s *Store) Update(in Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(in.ID)
	if i < 0 {
		return ErrNotFound
	}
	if in.Password == "" {
		in.Password = s.instances[i].Password
	}
	if in.AgentToken == "" {
		in.AgentToken = s.instances[i].AgentToken
	}
	in = normalize(in)
	if err := validate(in); err != nil {
		return err
	}
	next := append([]Instance(nil), s.instances...)
	next[i] = in
	if err := s.save(next, s.users); err != nil {
		return err
	}
	s.instances = next
	return nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return ErrNotFound
	}
	next := append(append([]Instance(nil), s.instances[:i]...), s.instances[i+1:]...)
	if err := s.save(next, s.users); err != nil {
		return err
	}
	s.instances = next
	return nil
}

func (s *Store) index(id string) int {
	for i, in := range s.instances {
		if in.ID == id {
			return i
		}
	}
	return -1
}

// Users returns a copy of the accounts in insertion order.
func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]User(nil), s.users...)
}

func (s *Store) User(id string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.userIndex(func(u User) bool { return u.ID == id }); i >= 0 {
		return s.users[i], true
	}
	return User{}, false
}

func (s *Store) UserByName(username string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.userIndex(func(u User) bool { return u.Username == username }); i >= 0 {
		return s.users[i], true
	}
	return User{}, false
}

// AddUser assigns a new ID and, when unset, the creation time.
func (s *Store) AddUser(u User) (User, error) {
	if err := validateUser(u); err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userIndex(func(o User) bool { return o.Username == u.Username }) >= 0 {
		return User{}, ErrDuplicate
	}
	for {
		id, err := newID()
		if err != nil {
			return User{}, err
		}
		if s.userIndex(func(o User) bool { return o.ID == id }) < 0 {
			u.ID = id
			break
		}
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC().Truncate(time.Second)
	}
	next := append(append([]User(nil), s.users...), u)
	if err := s.save(s.instances, next); err != nil {
		return User{}, err
	}
	s.users = next
	return u, nil
}

// UpdateUser replaces the account with the same ID and keeps its creation
// time.
func (s *Store) UpdateUser(u User) error {
	if err := validateUser(u); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.userIndex(func(o User) bool { return o.ID == u.ID })
	if i < 0 {
		return ErrUserNotFound
	}
	if j := s.userIndex(func(o User) bool { return o.Username == u.Username }); j >= 0 && j != i {
		return ErrDuplicate
	}
	u.CreatedAt = s.users[i].CreatedAt
	next := append([]User(nil), s.users...)
	next[i] = u
	if err := s.save(s.instances, next); err != nil {
		return err
	}
	s.users = next
	return nil
}

func (s *Store) DeleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.userIndex(func(u User) bool { return u.ID == id })
	if i < 0 {
		return ErrUserNotFound
	}
	next := append(append([]User(nil), s.users[:i]...), s.users[i+1:]...)
	if err := s.save(s.instances, next); err != nil {
		return err
	}
	s.users = next
	return nil
}

func (s *Store) userIndex(match func(User) bool) int {
	for i, u := range s.users {
		if match(u) {
			return i
		}
	}
	return -1
}

// save writes a temp file in the target directory and renames it over the
// store, so a crash never leaves a truncated file. CreateTemp uses mode 0600.
func (s *Store) save(instances []Instance, users []User) error {
	out := file{Instances: append([]Instance{}, instances...), Users: users}
	if s.aead != nil {
		for i := range out.Instances {
			var err error
			in := &out.Instances[i]
			if in.Password, err = s.seal(in.Password); err != nil {
				return err
			}
			if in.AgentToken != "" {
				if in.AgentToken, err = s.seal(in.AgentToken); err != nil {
					return err
				}
			}
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// normalize drops the config access fields the mode does not use.
func normalize(in Instance) Instance {
	if in.ConfigMode != ConfigLocal {
		in.ConfigPath = ""
	}
	if in.ConfigMode != ConfigAgent {
		in.AgentURL, in.AgentToken = "", ""
	}
	return in
}

func validate(in Instance) error {
	switch n := utf8.RuneCountInString(in.Name); {
	case n < 1 || n > 64:
		return fmt.Errorf("%w: name must be 1 to 64 characters", ErrInvalid)
	case in.Host == "" || strings.ContainsFunc(in.Host, unicode.IsSpace):
		return fmt.Errorf("%w: host must be non-empty and contain no spaces", ErrInvalid)
	case in.Port < 1 || in.Port > 65535:
		return fmt.Errorf("%w: port must be 1 to 65535", ErrInvalid)
	}
	switch in.ConfigMode {
	case "":
	case ConfigLocal:
		if !filepath.IsAbs(in.ConfigPath) || filepath.Clean(in.ConfigPath) != in.ConfigPath {
			return fmt.Errorf("%w: configPath must be a clean absolute path", ErrInvalid)
		}
	case ConfigAgent:
		u, err := url.Parse(in.AgentURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%w: agentURL must be an http or https URL without credentials, query or fragment", ErrInvalid)
		}
		if n := utf8.RuneCountInString(in.AgentToken); n < 16 || n > 256 || strings.ContainsAny(in.AgentToken, "\r\n") {
			return fmt.Errorf("%w: agentToken must be 16 to 256 characters on one line", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: configMode must be empty, local or agent", ErrInvalid)
	}
	return nil
}

func validateUser(u User) error {
	if err := CheckUsername(u.Username); err != nil {
		return err
	}
	if err := CheckRole(u.Role); err != nil {
		return err
	}
	if u.PasswordHash == "" {
		return fmt.Errorf("%w: password hash is required", ErrInvalidUser)
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
