//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package conffs

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "agent-token-0123456789"

func newAgent(t *testing.T) (fixture, *Client, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	ts := httptest.NewServer(NewAgentHandler(f.l, testToken))
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL+"/", testToken)
	if err != nil {
		t.Fatal(err)
	}
	return f, c, ts
}

func TestAgentToken(t *testing.T) {
	_, _, ts := newAgent(t)
	for _, auth := range []string{"", "Bearer", "Bearer ", "bearer " + testToken, "Bearer " + testToken + "x", "Basic " + testToken, "Bearer " + testToken[:10]} {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/files?path=", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 || strings.TrimSpace(string(body)) != `{"error":"invalid token"}` || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%q: %d %s", auth, resp.StatusCode, body)
		}
	}
	for path, code := range map[string]int{"/v1/files?path=": 200, "/v1/other": 404, "/": 404, "/debug/pprof/": 404} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestAgentRoundTrip(t *testing.T) {
	f, c, _ := newAgent(t)
	rows, err := c.List(ctx, "sip_profiles")
	if err != nil || len(rows) != 2 || rows[1].Name != "internal.xml" {
		t.Fatalf("list %+v %v", rows, err)
	}
	vars, err := c.Read(ctx, "vars.xml")
	if err != nil || !strings.Contains(vars.Content, "X-PRE-PROCESS") || len(vars.SHA256) != 64 {
		t.Fatalf("read %+v %v", vars, err)
	}
	res, err := c.Write(ctx, "vars.xml", "<include/>", vars.SHA256, "alice")
	if err != nil || res.Path != "vars.xml" || res.Size != 10 {
		t.Fatalf("write %+v %v", res, err)
	}
	var ce *ConflictError
	if _, err := c.Write(ctx, "vars.xml", "<include/>", vars.SHA256, "alice"); !errors.As(err, &ce) || ce.Current != res.SHA256 {
		t.Fatalf("conflict over the agent: %v", err)
	}
	var se *SyntaxError
	if _, err := c.Write(ctx, "vars.xml", "<a>\n</b>", res.SHA256, "alice"); !errors.As(err, &se) || se.Line != 2 || strings.HasPrefix(se.Msg, syntaxPrefix) {
		t.Fatalf("syntax over the agent: %#v", err)
	}
	if _, err := c.Read(ctx, "../secret.xml"); !errors.Is(err, ErrInvalid) || err.Error() != "path must not contain .." {
		t.Fatalf("invalid over the agent: %v", err)
	}
	if _, err := c.Read(ctx, "out.xml"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("link out over the agent: %v", err)
	}
	if _, err := c.Read(ctx, "nope.xml"); !errors.Is(err, ErrNotFound) || err.Error() != "file not found" {
		t.Fatalf("not found over the agent: %v", err)
	}
	hist, err := c.History(ctx, "vars.xml")
	if err != nil || len(hist) != 1 || hist[0].User != "alice" {
		t.Fatalf("history %+v %v", hist, err)
	}
	b, err := c.Backup(ctx, "vars.xml", hist[0].ID)
	if err != nil || b.Content != vars.Content {
		t.Fatalf("backup %+v %v", b, err)
	}
	if _, err := c.Restore(ctx, "vars.xml", hist[0].ID, res.SHA256, "bob"); err != nil {
		t.Fatal(err)
	}
	cur, _ := f.l.Read(ctx, "vars.xml")
	if cur.Content != vars.Content {
		t.Fatal("restore over the agent")
	}
	if err := c.Delete(ctx, "vars.xml", cur.SHA256, "bob"); err != nil {
		t.Fatal(err)
	}
	if h, _ := c.History(ctx, "vars.xml"); len(h) != 3 || h[0].User != "bob" {
		t.Fatalf("history %+v", h)
	}
	if h, err := c.History(ctx, "never.xml"); err != nil || h == nil || len(h) != 0 {
		t.Fatalf("empty history %v %v", h, err)
	}
}

func TestClientErrors(t *testing.T) {
	_, _, ts := newAgent(t)
	wrong, _ := NewClient(ts.URL, "another-token-value-x")
	if _, err := wrong.List(ctx, ""); !errors.Is(err, ErrAgent) || !strings.Contains(err.Error(), "rejected the token") || strings.Contains(err.Error(), "another-token") {
		t.Fatalf("wrong token: %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	down, _ := NewClient(closedURL, testToken)
	if _, err := down.Read(ctx, "vars.xml"); !errors.Is(err, ErrAgent) || !strings.Contains(err.Error(), "cannot reach the agent") {
		t.Fatalf("unreachable: %v", err)
	}

	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	notAgent, _ := NewClient(other.URL, testToken)
	if _, err := notAgent.List(ctx, ""); !errors.Is(err, ErrAgent) || !strings.Contains(err.Error(), "no fsdeck agent") {
		t.Fatalf("not an agent: %v", err)
	}

	// TLS is verified: a certificate the system does not trust is refused.
	tlsAgent := httptest.NewTLSServer(NewAgentHandler(newFixture(t).l, testToken))
	defer tlsAgent.Close()
	untrusted, _ := NewClient(tlsAgent.URL, testToken)
	if _, err := untrusted.List(ctx, ""); !errors.Is(err, ErrAgent) || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted certificate: %v", err)
	}

	// Redirects are not followed, so the token stays with the agent.
	var leaked bool
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("Authorization") != ""
	}))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirect.Close()
	r, _ := NewClient(redirect.URL, testToken)
	if _, err := r.List(ctx, ""); !errors.Is(err, ErrAgent) || leaked {
		t.Fatalf("redirect: %v, leaked %v", err, leaked)
	}

	for _, u := range []string{"", "ftp://h", "http://", "://x"} {
		if _, err := NewClient(u, testToken); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestWriteError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		body string
	}{
		{invalid("bad path"), 400, `{"error":"bad path"}`},
		{notFound("file"), 404, `{"error":"file not found"}`},
		{&ConflictError{Current: "abc"}, 409, `{"error":"the file has changed since it was read","currentSha256":"abc"}`},
		{&ConflictError{}, 409, `{"error":"the file does not exist (any more)","currentSha256":""}`},
		{&SyntaxError{Line: 7, Msg: "unexpected EOF"}, 422, `{"error":"the XML is not well-formed: unexpected EOF","line":7}`},
		{errors.Join(ErrAgent, errors.New("x")), 502, ""},
		{errors.New("disk full"), 500, `{"error":"disk full"}`},
	} {
		rec := httptest.NewRecorder()
		WriteError(rec, tc.err)
		if rec.Code != tc.code || tc.body != "" && strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Errorf("%v: %d %s", tc.err, rec.Code, rec.Body)
		}
	}
}
