package conffs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the FileStore of a FreeSWITCH host reached through its agent.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// agentHTTP is shared by all clients. TLS certificates are verified with
// the system roots; redirects are not followed, so the token never goes
// anywhere but the configured agent.
var agentHTTP = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// NewClient talks to the agent at agentURL (http or https) with token.
func NewClient(agentURL, token string) (*Client, error) {
	u, err := url.Parse(agentURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: agent URL must be an http or https URL", ErrAgent)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &Client{base: u, token: token, http: agentHTTP}, nil
}

// do sends one request and decodes a 2xx answer into out; any other answer
// becomes the matching error.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	u := *c.base
	u.Path += path
	u.RawQuery = q.Encode()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL carries no secret; the error names the host only.
		return fmt.Errorf("%w: cannot reach the agent at %s: %v", ErrAgent, c.base.Host, unwrapURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: reading the answer: %v", ErrAgent, err)
	}
	if resp.StatusCode/100 == 2 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%w: unexpected answer: %v", ErrAgent, err)
		}
		return nil
	}
	var e errorBody
	json.Unmarshal(data, &e)
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return &kindError{ErrInvalid, e.Error}
	case http.StatusNotFound:
		if e.Error == "" || e.Error == "not found" {
			return fmt.Errorf("%w: no fsconsole agent at %s (404)", ErrAgent, c.base.Host)
		}
		return &kindError{ErrNotFound, e.Error}
	case http.StatusConflict:
		if e.CurrentSHA256 != nil {
			return &ConflictError{Current: *e.CurrentSHA256}
		}
	case http.StatusUnprocessableEntity:
		if e.Line != nil {
			return &SyntaxError{Line: *e.Line, Msg: strings.TrimPrefix(e.Error, syntaxPrefix)}
		}
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: the agent rejected the token", ErrAgent)
	}
	if e.Error == "" {
		e.Error = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("%w: %d %s", ErrAgent, resp.StatusCode, e.Error)
}

// unwrapURL drops the *url.Error wrapper, which repeats the method and URL.
func unwrapURL(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	var out struct{ Rows []Entry }
	err := c.do(ctx, "GET", "/v1/files", url.Values{"path": {dir}}, nil, &out)
	if out.Rows == nil {
		out.Rows = []Entry{}
	}
	return out.Rows, err
}

func (c *Client) Read(ctx context.Context, path string) (File, error) {
	var f File
	return f, c.do(ctx, "GET", "/v1/files/content", url.Values{"path": {path}}, nil, &f)
}

func (c *Client) Write(ctx context.Context, path, content, baseSHA256, user string) (Written, error) {
	var res Written
	return res, c.do(ctx, "PUT", "/v1/files/content", nil, agentWrite{Path: path, Content: content, BaseSHA256: baseSHA256, User: user}, &res)
}

func (c *Client) Delete(ctx context.Context, path, baseSHA256, user string) error {
	return c.do(ctx, "DELETE", "/v1/files/content", url.Values{"path": {path}, "baseSha256": {baseSHA256}, "user": {user}}, nil, nil)
}

func (c *Client) History(ctx context.Context, path string) ([]Backup, error) {
	var out struct{ Rows []Backup }
	err := c.do(ctx, "GET", "/v1/files/history", url.Values{"path": {path}}, nil, &out)
	if out.Rows == nil {
		out.Rows = []Backup{}
	}
	return out.Rows, err
}

func (c *Client) Backup(ctx context.Context, path, id string) (BackupContent, error) {
	var b BackupContent
	return b, c.do(ctx, "GET", "/v1/files/backup", url.Values{"path": {path}, "id": {id}}, nil, &b)
}

func (c *Client) Restore(ctx context.Context, path, id, baseSHA256, user string) (Written, error) {
	var res Written
	return res, c.do(ctx, "POST", "/v1/files/restore", nil, agentRestore{Path: path, ID: id, BaseSHA256: baseSHA256, User: user}, &res)
}

func (c *Client) Search(ctx context.Context, query string, opts SearchOptions) (SearchResult, error) {
	if opts.Limit == 0 {
		opts.Limit = DefaultSearchLimit
	}
	var res SearchResult
	err := c.do(ctx, "GET", "/v1/search", opts.values(query), nil, &res)
	if res.Rows == nil {
		res.Rows = []Match{}
	}
	return res, err
}
