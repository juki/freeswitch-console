// Package store keeps the list of FreeSWITCH instances in a JSON file.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Instance is one FreeSWITCH reachable over inbound ESL.
type Instance struct {
	ID       string `json:"id"` // 16 hex chars, generated
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"` // ESL password, stored in clear
}

var (
	ErrNotFound = errors.New("instance not found")
	ErrInvalid  = errors.New("invalid instance")
)

// Store is safe for concurrent use.
type Store struct {
	path      string
	mu        sync.Mutex
	instances []Instance
}

type file struct {
	Instances []Instance `json:"instances"`
}

// Open loads the store at path; a missing file is an empty store.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
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
	s.instances = f.Instances
	return s, nil
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
	if err := s.save(next); err != nil {
		return Instance{}, err
	}
	s.instances = next
	return in, nil
}

// Update replaces the instance with the same ID; an empty Password keeps the
// stored one.
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
	if err := validate(in); err != nil {
		return err
	}
	next := append([]Instance(nil), s.instances...)
	next[i] = in
	if err := s.save(next); err != nil {
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
	if err := s.save(next); err != nil {
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

// save writes a temp file in the target directory and renames it over the
// store, so a crash never leaves a truncated file. CreateTemp uses mode 0600.
func (s *Store) save(list []Instance) error {
	if list == nil {
		list = []Instance{}
	}
	data, err := json.MarshalIndent(file{Instances: list}, "", "  ")
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

func validate(in Instance) error {
	switch n := utf8.RuneCountInString(in.Name); {
	case n < 1 || n > 64:
		return fmt.Errorf("%w: name must be 1 to 64 characters", ErrInvalid)
	case in.Host == "" || strings.ContainsFunc(in.Host, unicode.IsSpace):
		return fmt.Errorf("%w: host must be non-empty and contain no spaces", ErrInvalid)
	case in.Port < 1 || in.Port > 65535:
		return fmt.Errorf("%w: port must be 1 to 65535", ErrInvalid)
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
