// Command fsconsole serves a web console for operating FreeSWITCH over ESL.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/juki/freeswitch-console/internal/audit"
	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/server"
	"github.com/juki/freeswitch-console/internal/store"
	"github.com/juki/freeswitch-console/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("listen", envOr("FSCONSOLE_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	data := flag.String("data", envOr("FSCONSOLE_DATA", "fsconsole.json"), "data file: instances and accounts")
	static := flag.String("static", "", "serve the frontend from this directory instead of the embedded copy (frontend development)")
	tlsCert := flag.String("tls-cert", os.Getenv("FSCONSOLE_TLS_CERT"), "TLS certificate file (PEM); with -tls-key the console serves HTTPS")
	tlsKey := flag.String("tls-key", os.Getenv("FSCONSOLE_TLS_KEY"), "TLS private key file (PEM)")
	backlog := flag.Int("log-backlog", envInt("FSCONSOLE_LOG_BACKLOG", 2000), "log lines kept per instance and replayed when the Logs view opens; 0 disables")
	backlogLevel := flag.Int("log-backlog-level", envInt("FSCONSOLE_LOG_BACKLOG_LEVEL", 6), "highest log level kept in the backlog, 0 (console) to 7 (debug)")
	auditPath := flag.String("audit", "", "audit log file (default fsconsole-audit.log beside the data file; empty disables)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if !flagSet("audit") {
		// An empty FSCONSOLE_AUDIT disables the log, so unset and empty differ.
		if v, ok := os.LookupEnv("FSCONSOLE_AUDIT"); ok {
			*auditPath = v
		} else {
			*auditPath = filepath.Join(filepath.Dir(*data), "fsconsole-audit.log")
		}
	}

	tlsConfig, err := loadTLS(*tlsCert, *tlsKey)
	if err != nil {
		log.Fatal(err)
	}

	var assets fs.FS = web.Static()
	if *static != "" {
		assets = os.DirFS(*static)
	}

	var opts []store.Option
	if key := os.Getenv("FSCONSOLE_SECRET_KEY"); key != "" {
		opts = append(opts, store.WithSecretKey(key))
	} else {
		log.Printf("warning: FSCONSOLE_SECRET_KEY is not set; ESL passwords are stored in clear in %s", *data)
	}
	st, err := store.Open(*data, opts...)
	if err != nil {
		log.Fatalf("open %s: %v", *data, err)
	}

	adminPassword := os.Getenv("FSCONSOLE_ADMIN_PASSWORD")
	password, err := server.Bootstrap(st, adminPassword != "")
	if err != nil {
		log.Fatalf("create the admin account: %v", err)
	}
	if password != "" {
		log.Printf("created the account admin with password %s (shown only now; change it after signing in)", password)
	}

	var auditLog *audit.Log
	if *auditPath != "" {
		if auditLog, err = audit.Open(*auditPath); err != nil {
			log.Fatalf("audit log: %v", err)
		}
		defer auditLog.Close()
	}

	app, err := server.New(server.Config{
		AdminPassword:   adminPassword,
		ViewerPassword:  os.Getenv("FSCONSOLE_VIEWER_PASSWORD"),
		Store:           st,
		Dial:            dial,
		Static:          assets,
		Version:         version,
		LogBacklog:      *backlog,
		LogBacklogLevel: *backlogLevel,
		Audit:           auditLog,
		BackupDir:       filepath.Join(filepath.Dir(*data), "backups"),
	})
	if err != nil {
		log.Fatal(err)
	}

	// No WriteTimeout: the event stream is a long-lived response.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Closing the switches ends the open event streams, which would
		// otherwise keep Shutdown waiting until its deadline.
		app.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			srv.Close()
		}
	}()

	if tlsConfig != nil {
		log.Printf("fsconsole %s listening on https://%s", version, *listen)
		err = srv.ListenAndServeTLS("", "")
	} else {
		log.Printf("fsconsole %s listening on http://%s", version, *listen)
		err = srv.ListenAndServe()
	}
	if !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// loadTLS returns nil when neither file is given. The pair is loaded at start
// so a bad path or key fails before the console is up.
func loadTLS(certFile, keyFile string) (*tls.Config, error) {
	switch {
	case certFile == "" && keyFile == "":
		return nil, nil
	case certFile == "" || keyFile == "":
		return nil, errors.New("-tls-cert and -tls-key (FSCONSOLE_TLS_CERT, FSCONSOLE_TLS_KEY) must be given together")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}

func dial(addr, password string) server.Switch {
	c := esl.NewClient(addr, password)
	c.Start()
	return c
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envInt reads an integer from the environment; a value that is not one
// stops the start.
func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: %q is not a number", name, v)
	}
	return n
}

func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}
