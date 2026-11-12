// Command fsdeck serves a web console for operating FreeSWITCH over ESL.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fsdeck/internal/esl"
	"fsdeck/internal/server"
	"fsdeck/internal/store"
	"fsdeck/web"
)

func main() {
	listen := flag.String("listen", envOr("FSDECK_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	data := flag.String("data", envOr("FSDECK_DATA", "fsdeck.json"), "path of the instance list file")
	flag.Parse()

	adminPassword := os.Getenv("FSDECK_ADMIN_PASSWORD")
	if adminPassword == "" {
		adminPassword = randomPassword()
		log.Printf("FSDECK_ADMIN_PASSWORD is not set; admin password for this run: %s", adminPassword)
	}

	st, err := store.Open(*data)
	if err != nil {
		log.Fatalf("open %s: %v", *data, err)
	}

	app, err := server.New(server.Config{
		AdminPassword:  adminPassword,
		ViewerPassword: os.Getenv("FSDECK_VIEWER_PASSWORD"),
		Store:          st,
		Dial:           dial,
		Static:         web.Static(),
	})
	if err != nil {
		log.Fatal(err)
	}

	// No WriteTimeout: the event stream is a long-lived response.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
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

	log.Printf("fsdeck listening on http://%s", *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
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

func randomPassword() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}
