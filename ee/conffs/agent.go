//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package conffs

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxBody bounds a request: a file of MaxSize, JSON-escaped.
const maxBody = 8 << 20

// NewAgentHandler serves store under /v1/ to fsdeck. Every request needs
// "Authorization: Bearer <token>"; nothing else is exposed.
func NewAgentHandler(store FileStore, token string) http.Handler {
	want := sha256.Sum256([]byte(token))
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/files", func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.List(r.Context(), r.URL.Query().Get("path"))
		answer(w, map[string]any{"rows": rows}, err)
	})
	m.HandleFunc("GET /v1/files/content", func(w http.ResponseWriter, r *http.Request) {
		f, err := store.Read(r.Context(), r.URL.Query().Get("path"))
		answer(w, f, err)
	})
	m.HandleFunc("PUT /v1/files/content", func(w http.ResponseWriter, r *http.Request) {
		var req agentWrite
		if !decode(w, r, &req) {
			return
		}
		res, err := store.Write(r.Context(), req.Path, req.Content, req.BaseSHA256, req.User)
		answer(w, res, err)
	})
	m.HandleFunc("DELETE /v1/files/content", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if err := store.Delete(r.Context(), q.Get("path"), q.Get("baseSha256"), q.Get("user")); err != nil {
			WriteError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("GET /v1/files/history", func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.History(r.Context(), r.URL.Query().Get("path"))
		answer(w, map[string]any{"rows": rows}, err)
	})
	m.HandleFunc("GET /v1/files/backup", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		b, err := store.Backup(r.Context(), q.Get("path"), q.Get("id"))
		answer(w, b, err)
	})
	m.HandleFunc("POST /v1/files/restore", func(w http.ResponseWriter, r *http.Request) {
		var req agentRestore
		if !decode(w, r, &req) {
			return
		}
		res, err := store.Restore(r.Context(), req.Path, req.ID, req.BaseSHA256, req.User)
		answer(w, res, err)
	})
	m.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not found"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Hashing first keeps the comparison constant in time and length.
		have := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(have[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fsdeck-agent"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "invalid token"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		m.ServeHTTP(w, r)
	})
}

type agentWrite struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	BaseSHA256 string `json:"baseSha256"`
	User       string `json:"user"`
}

type agentRestore struct {
	Path       string `json:"path"`
	ID         string `json:"id"`
	BaseSHA256 string `json:"baseSha256"`
	User       string `json:"user"`
}

func answer(w http.ResponseWriter, v any, err error) {
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("unexpected data after JSON value")
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}
