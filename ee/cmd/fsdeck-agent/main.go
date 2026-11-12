//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

// Command fsdeck-agent runs on a FreeSWITCH host and lets fsdeck read and
// write the configuration files below one directory, with backups kept on
// that host. It serves only /v1/ and every request needs the bearer token.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"fsdeck/ee/conffs"
)

func main() {
	srv, err := setup(os.Args[1:], os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if srv.TLSConfig != nil {
		log.Printf("fsdeck-agent listening on https://%s", srv.Addr)
		err = srv.ListenAndServeTLS("", "")
	} else {
		log.Printf("fsdeck-agent listening on http://%s (no TLS: use it on a trusted network or through a tunnel)", srv.Addr)
		err = srv.ListenAndServe()
	}
	if !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// setup checks the flags and returns the server; it refuses to start
// without a usable token.
func setup(args []string, stderr io.Writer) (*http.Server, error) {
	fs := flag.NewFlagSet("fsdeck-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8025", "listen address")
	root := fs.String("root", "/etc/freeswitch", "FreeSWITCH configuration directory")
	backups := fs.String("backups", "/var/lib/fsdeck-agent/backups", "backup directory, outside -root")
	tokenFile := fs.String("token-file", "", "file holding the bearer token (at least 16 characters); required")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (PEM); with -tls-key the agent serves HTTPS")
	tlsKey := fs.String("tls-key", "", "TLS private key file (PEM)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *tokenFile == "" {
		return nil, errors.New("-token-file is required: the agent never runs without a token")
	}
	data, err := os.ReadFile(*tokenFile)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 16 || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("token: the token file must hold one line of at least 16 characters")
	}
	if st, err := os.Stat(*tokenFile); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(stderr, "warning: %s can be read by other users; chmod 600 it\n", *tokenFile)
	}
	store, err := conffs.NewLocal(*root, *backups)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           conffs.NewAgentHandler(store, token),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	switch {
	case *tlsCert == "" && *tlsKey == "":
	case *tlsCert == "" || *tlsKey == "":
		return nil, errors.New("-tls-cert and -tls-key must be given together")
	default:
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	return srv, nil
}
