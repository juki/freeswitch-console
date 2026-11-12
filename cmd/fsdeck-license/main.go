// Command fsdeck-license creates signing keys and issues and checks fsdeck
// licence keys.
//
//	fsdeck-license keygen [-out private.key]
//	fsdeck-license sign -key private.key -licensee NAME [-plan PLAN] -features users,audit,config-edit [-days N] [-id ID]
//	fsdeck-license verify [-pub BASE64] KEY-OR-FILE
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"fsdeck/internal/license"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "fsdeck-license:", err)
		os.Exit(1)
	}
}

const usage = "usage: fsdeck-license keygen [-out FILE] | sign -key FILE -licensee NAME [-plan PLAN] -features a,b [-days N] [-id ID] | verify [-pub BASE64] KEY-OR-FILE"

func run(args []string, out io.Writer, now time.Time) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch args[0] {
	case "keygen":
		file := fs.String("out", "", "write the private key to this file (mode 0600) instead of printing it")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New(usage)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		privText := base64.StdEncoding.EncodeToString(priv)
		if *file != "" {
			// O_EXCL: never overwrite an existing signing key.
			f, err := os.OpenFile(*file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(f, privText); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			fmt.Fprintln(out, "private", privText)
		}
		fmt.Fprintln(out, "public", base64.StdEncoding.EncodeToString(pub))
		return nil

	case "sign":
		keyFile := fs.String("key", "", "file holding the base64 private key")
		licensee := fs.String("licensee", "", "who the licence is for")
		plan := fs.String("plan", "", "plan name, free text")
		features := fs.String("features", "", "comma-separated: "+strings.Join(license.Features, ","))
		days := fs.Int("days", 365, "days until it expires; 0 never expires")
		id := fs.String("id", "", "licence id (default: random)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *keyFile == "" || strings.TrimSpace(*licensee) == "" || *days < 0 {
			return errors.New(usage)
		}
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		priv, err := license.ParsePrivateKey(string(data))
		if err != nil {
			return err
		}
		l := license.License{ID: *id, Licensee: strings.TrimSpace(*licensee), Plan: *plan, Features: []string{}, Issued: now.Unix()}
		for _, f := range strings.Split(*features, ",") {
			if f = strings.TrimSpace(f); f == "" {
				continue
			}
			if !slices.Contains(license.Features, f) {
				return fmt.Errorf("unknown feature %q (known: %s)", f, strings.Join(license.Features, ", "))
			}
			if !slices.Contains(l.Features, f) {
				l.Features = append(l.Features, f)
			}
		}
		if len(l.Features) == 0 {
			return errors.New("-features names no feature")
		}
		if *days > 0 {
			l.Expires = now.Add(time.Duration(*days) * 24 * time.Hour).Unix()
		}
		if l.ID == "" {
			b := make([]byte, 8)
			rand.Read(b)
			l.ID = hex.EncodeToString(b)
		}
		key, err := license.Sign(l, priv)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, key)
		return nil

	case "verify":
		pubText := fs.String("pub", "", "base64 public key (default: the one built into fsdeck)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return errors.New(usage)
		}
		key := fs.Arg(0)
		if data, err := os.ReadFile(key); err == nil {
			key = string(data)
		}
		var k license.Key
		if *pubText == "" {
			k = license.Load(key)
		} else {
			pub, err := license.ParsePublicKey(*pubText)
			if err != nil {
				return err
			}
			k = license.Check(key, pub)
		}
		status := k.Status(now)
		if l, ok := k.License(); ok {
			b, _ := json.MarshalIndent(l, "", "  ")
			fmt.Fprintln(out, string(b))
		}
		fmt.Fprintln(out, "status", status)
		switch status {
		case "valid":
			return nil
		case "invalid", "none":
			if k.Err() != nil {
				return k.Err()
			}
		}
		return errors.New("licence is " + status)
	}
	return errors.New(usage)
}
