package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets a test run the real main in a child process of the test
// binary, so start-up behaviour is checked end to end.
func TestMain(m *testing.M) {
	if os.Getenv("FSCONSOLE_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMain starts main with args and env; the caller must Wait or Kill.
func runMain(t *testing.T, env []string, args ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append([]string{"FSCONSOLE_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd, bufio.NewScanner(out)
}

// startupOutput returns everything main prints until it listens or exits.
func startupOutput(t *testing.T, env []string, args ...string) (string, bool) {
	t.Helper()
	cmd, sc := runMain(t, env, args...)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if strings.Contains(sc.Text(), "listening on") {
			cmd.Process.Kill()
			return strings.Join(lines, "\n"), true
		}
	}
	return strings.Join(lines, "\n"), false
}

func TestVersionFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-version")
	cmd.Env = []string{"FSCONSOLE_TEST_RUN_MAIN=1"}
	// Only stdout: under -cover the child warns on stderr that GOCOVERDIR
	// is not set.
	out, err := cmd.Output()
	if err != nil || string(out) != "dev\n" {
		t.Fatalf("-version: %v %q", err, out)
	}
}

func TestSecretKeyStartup(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "fsconsole.json")
	os.WriteFile(data, []byte(`{"instances":[{"id":"0123456789abcdef","name":"a","host":"127.0.0.1","port":1,"password":"ClueCon"}]}`), 0o600)
	base := []string{"FSCONSOLE_DATA=" + data, "FSCONSOLE_LISTEN=127.0.0.1:0", "FSCONSOLE_ADMIN_PASSWORD=admin-password", "FSCONSOLE_AUDIT="}

	// No key: one warning, then it runs.
	out, ok := startupOutput(t, base)
	if !ok || strings.Count(out, "FSCONSOLE_SECRET_KEY is not set") != 1 {
		t.Fatalf("no key:\n%s", out)
	}

	// With a key: runs without the warning; the file is encrypted on the
	// next save, not at start.
	key := "FSCONSOLE_SECRET_KEY=0123456789abcdef-key"
	out, ok = startupOutput(t, append(base, key))
	if !ok || strings.Contains(out, "FSCONSOLE_SECRET_KEY") {
		t.Fatalf("with key:\n%s", out)
	}

	// Encrypted file, then missing, wrong and short keys.
	os.WriteFile(data, []byte(`{"instances":[{"id":"0123456789abcdef","name":"a","host":"127.0.0.1","port":1,"password":"enc:v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}`), 0o600)
	for env, want := range map[string]string{
		"":                                      "no secret key is set (FSCONSOLE_SECRET_KEY)",
		key:                                     "the secret key (FSCONSOLE_SECRET_KEY) is wrong",
		"FSCONSOLE_SECRET_KEY=short":            "must be at least 16 characters",
		"FSCONSOLE_SECRET_KEY=x-16-characters!": "is wrong",
	} {
		out, ok := startupOutput(t, append(base, env))
		if ok || !strings.Contains(out, want) {
			t.Errorf("%q:\n%s", env, out)
		}
	}
}

func TestLoadTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir)
	if c, err := loadTLS("", ""); c != nil || err != nil {
		t.Fatalf("neither: %v %v", c, err)
	}
	for _, pair := range [][2]string{{certFile, ""}, {"", keyFile}} {
		if _, err := loadTLS(pair[0], pair[1]); err == nil || !strings.Contains(err.Error(), "together") {
			t.Errorf("%q: %v", pair, err)
		}
	}
	if _, err := loadTLS(filepath.Join(dir, "missing.pem"), keyFile); err == nil {
		t.Error("missing certificate accepted")
	}
	if _, err := loadTLS(keyFile, certFile); err == nil {
		t.Error("swapped files accepted")
	}
	c, err := loadTLS(certFile, keyFile)
	if err != nil || len(c.Certificates) != 1 || c.MinVersion == 0 {
		t.Fatalf("valid pair: %v %+v", err, c)
	}

	// main refuses to start with only one of them.
	out, ok := startupOutput(t, []string{"FSCONSOLE_TLS_CERT=" + certFile, "FSCONSOLE_LISTEN=127.0.0.1:0"})
	if ok || !strings.Contains(out, "must be given together") {
		t.Fatalf("one file:\n%s", out)
	}
}

func writeCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return certFile, keyFile
}

func TestBootstrapAndAuditDefault(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "fsconsole.json")
	env := []string{"FSCONSOLE_DATA=" + data, "FSCONSOLE_LISTEN=127.0.0.1:0", "FSCONSOLE_SECRET_KEY=0123456789abcdef-key"}

	// First start without FSCONSOLE_ADMIN_PASSWORD: admin is created and its
	// password printed once.
	out, ok := startupOutput(t, env)
	if !ok || strings.Count(out, "created the account admin with password ") != 1 {
		t.Fatalf("first start:\n%s", out)
	}
	password := strings.Fields(strings.SplitN(out, "created the account admin with password ", 2)[1])[0]
	if len(password) != 24 {
		t.Fatalf("password %q", password)
	}
	stored, _ := os.ReadFile(data)
	if !strings.Contains(string(stored), `"username": "admin"`) || strings.Contains(string(stored), password) {
		t.Fatalf("data file %s", stored)
	}
	if _, err := os.Stat(filepath.Join(dir, "fsconsole-audit.log")); err != nil {
		t.Fatalf("audit log not beside the data file: %v", err)
	}

	// Second start: nothing created, nothing printed.
	out, ok = startupOutput(t, env)
	if !ok || strings.Contains(out, "password") {
		t.Fatalf("second start:\n%s", out)
	}

	// An empty FSCONSOLE_AUDIT disables the log; -audit wins over it.
	other := t.TempDir()
	envOther := []string{"FSCONSOLE_DATA=" + filepath.Join(other, "d.json"), "FSCONSOLE_LISTEN=127.0.0.1:0", "FSCONSOLE_ADMIN_PASSWORD=admin-password", "FSCONSOLE_AUDIT="}
	if _, ok := startupOutput(t, envOther); !ok {
		t.Fatal("start with audit disabled")
	}
	if _, err := os.Stat(filepath.Join(other, "fsconsole-audit.log")); err == nil {
		t.Fatal("audit log written while disabled")
	}
	explicit := filepath.Join(other, "explicit.log")
	if _, ok := startupOutput(t, envOther, "-audit", explicit); !ok {
		t.Fatal("start with -audit")
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("-audit ignored: %v", err)
	}

	// Bad numbers and levels stop the start.
	for _, e := range []string{"FSCONSOLE_LOG_BACKLOG=lots", "FSCONSOLE_LOG_BACKLOG_LEVEL=9", "FSCONSOLE_LOG_BACKLOG=-1", "FSCONSOLE_RECENT_CALLS=lots", "FSCONSOLE_RECENT_CALLS=-1"} {
		if out, ok := startupOutput(t, append(envOther, e)); ok || out == "" {
			t.Errorf("%s:\n%s", e, out)
		}
	}
	if out, ok := startupOutput(t, envOther, "-recent-calls", "-5"); ok || !strings.Contains(out, "recent calls must not be negative") {
		t.Errorf("-recent-calls -5:\n%s", out)
	}
	if out, ok := startupOutput(t, append(envOther, "FSCONSOLE_RECENT_CALLS=0")); !ok {
		t.Errorf("FSCONSOLE_RECENT_CALLS=0:\n%s", out)
	}
}

// TestDialerEvents checks the subscription the clients of main send: the
// hang-up event only while recent calls are kept, the SIP subclasses only
// while SIP events are, after CUSTOM and last.
func TestDialerEvents(t *testing.T) {
	sip := " CUSTOM sofia::register sofia::unregister sofia::expire sofia::register_failure sofia::gateway_state sofia::gateway_add sofia::gateway_delete"
	for _, tc := range []struct {
		recentCalls, sipEvents int
		want                   string
	}{
		{500, 1000, "event json BACKGROUND_JOB HEARTBEAT CHANNEL_HANGUP_COMPLETE" + sip},
		{0, 1000, "event json BACKGROUND_JOB HEARTBEAT" + sip},
		{500, 0, "event json BACKGROUND_JOB HEARTBEAT CHANNEL_HANGUP_COMPLETE"},
		{0, 0, "event json BACKGROUND_JOB HEARTBEAT"},
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		sw := dialer(tc.recentCalls, tc.sipEvents)(ln.Addr().String(), "ClueCon")
		defer sw.Close()
		conn, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		io.WriteString(conn, "Content-Type: auth/request\n\n")
		var cmds []string
		for len(cmds) < 2 {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line = strings.TrimSpace(line); line == "" {
				continue
			}
			cmds = append(cmds, line)
			if len(cmds) == 1 {
				io.WriteString(conn, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
			}
		}
		if cmds[0] != "auth ClueCon" || cmds[1] != tc.want {
			t.Errorf("recent calls %d, SIP events %d: commands %q, want %q", tc.recentCalls, tc.sipEvents, cmds, tc.want)
		}
	}
}

// TestSipEventsAndMetricsFlags starts main with -sip-events and
// FSCONSOLE_SIP_EVENTS, and FSCONSOLE_METRICS_TOKEN, and checks they reach
// the server; bad values stop the start.
func TestSipEventsAndMetricsFlags(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "fsconsole.json")
	os.WriteFile(data, []byte(`{"instances":[{"id":"0123456789abcdef","name":"a","host":"127.0.0.1","port":1,"password":"ClueCon"}]}`), 0o600)
	base := []string{"FSCONSOLE_DATA=" + data, "FSCONSOLE_ADMIN_PASSWORD=admin-password", "FSCONSOLE_AUDIT=", "FSCONSOLE_SECRET_KEY=0123456789abcdef-key"}
	const token = "a-metrics-token-of-24ch"

	// start runs main on a free port until it listens and returns its URL.
	start := func(env []string, args ...string) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		_, sc := runMain(t, append(env, "FSCONSOLE_LISTEN="+addr), args...)
		var out []string
		for sc.Scan() {
			out = append(out, sc.Text())
			if strings.Contains(sc.Text(), "listening on") {
				go func() {
					for sc.Scan() {
					}
				}()
				return "http://" + addr
			}
		}
		t.Fatalf("no start:\n%s", strings.Join(out, "\n"))
		return ""
	}
	capacity := func(base string) int {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		c := &testClient{t: t, base: base, http: &http.Client{Jar: jar, Timeout: 5 * time.Second}}
		if code, body := c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "admin-password"}); code != http.StatusOK {
			t.Fatalf("login: %d %s", code, body)
		}
		var reply struct {
			Enabled  bool
			Capacity int
		}
		c.get("/api/instances/0123456789abcdef/sipevents", http.StatusOK, &reply)
		return reply.Capacity
	}
	metrics := func(base, auth string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+"/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	url := start(base)
	if n := capacity(url); n != 1000 {
		t.Errorf("default capacity %d", n)
	}
	if code := metrics(url, "Bearer "+token); code != http.StatusNotFound {
		t.Errorf("/metrics without FSCONSOLE_METRICS_TOKEN: %d", code)
	}
	url = start(append(base, "FSCONSOLE_SIP_EVENTS=7", "FSCONSOLE_METRICS_TOKEN="+token))
	if n := capacity(url); n != 7 {
		t.Errorf("FSCONSOLE_SIP_EVENTS=7: capacity %d", n)
	}
	if ok, bad := metrics(url, "Bearer "+token), metrics(url, ""); ok != http.StatusOK || bad != http.StatusUnauthorized {
		t.Errorf("/metrics: %d with the token, %d without", ok, bad)
	}
	if n := capacity(start(append(base, "FSCONSOLE_SIP_EVENTS=7"), "-sip-events", "9")); n != 9 {
		t.Errorf("-sip-events 9: capacity %d", n)
	}
	if n := capacity(start(append(base, "FSCONSOLE_SIP_EVENTS=0"))); n != 0 {
		t.Errorf("FSCONSOLE_SIP_EVENTS=0: capacity %d", n)
	}

	for _, tc := range []struct {
		env  []string
		args []string
		want string
	}{
		{[]string{"FSCONSOLE_SIP_EVENTS=lots"}, nil, `FSCONSOLE_SIP_EVENTS: "lots" is not a number`},
		{[]string{"FSCONSOLE_SIP_EVENTS=-1"}, nil, "SIP events must not be negative"},
		{nil, []string{"-sip-events", "-5"}, "SIP events must not be negative"},
		{[]string{"FSCONSOLE_METRICS_TOKEN=fifteen-chars-x"}, nil, "FSCONSOLE_METRICS_TOKEN) must be at least 16 characters"},
		{[]string{"FSCONSOLE_METRICS_TOKEN=x"}, []string{"-demo"}, "FSCONSOLE_METRICS_TOKEN) must be at least 16 characters"},
	} {
		if out, ok := startupOutput(t, append(append(base, "FSCONSOLE_LISTEN=127.0.0.1:0"), tc.env...), tc.args...); ok || !strings.Contains(out, tc.want) {
			t.Errorf("%q %q:\n%s", tc.env, tc.args, out)
		}
	}
}
