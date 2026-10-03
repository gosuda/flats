package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the management server's loopback address.
const DefaultURL = "http://127.0.0.1:7878"

// client talks to the Flats HTTP API.
type client struct {
	base string
	hc   *http.Client
}

// apiError is a non-2xx API response.
type apiError struct {
	Status int
	Body   errorBody
	Raw    []byte
}

func (e *apiError) Error() string {
	if e.Body.Error != "" {
		return e.Body.Error
	}
	return fmt.Sprintf("server returned %d %s", e.Status, http.StatusText(e.Status))
}

// unreachableError means the server could not be contacted at all.
type unreachableError struct {
	base string
	err  error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("cannot reach Flats at %s: %v\n(is the server running? start it with `flats install` or `flats serve`, or point --url / FLATS_URL at it)", e.base, e.err)
}

func (e *unreachableError) Unwrap() error { return e.err }

// response is a raw API reply.
type response struct {
	Status int
	Body   []byte
}

// do sends one request and returns the raw reply; it only fails when no
// reply was received.
func (c *client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string) (response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("X-Flats-Client", "cli")
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return response{}, ctx.Err()
		}
		return response{}, &unreachableError{base: c.base, err: errors.Unwrap(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return response{}, fmt.Errorf("read response: %w", err)
	}
	return response{Status: resp.StatusCode, Body: raw}, nil
}

// call sends JSON (when in != nil), and decodes a 2xx reply into out.
// Non-2xx replies become *apiError.
func (c *client) call(ctx context.Context, method, path string, q url.Values, in, out any) (response, error) {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return response{}, err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, q, body, ctype)
	if err != nil {
		return resp, err
	}
	if resp.Status >= 300 {
		return resp, newAPIError(resp)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, fmt.Errorf("unexpected reply from %s %s: %w", method, path, err)
		}
	}
	return resp, nil
}

func newAPIError(resp response) *apiError {
	e := &apiError{Status: resp.Status, Raw: resp.Body}
	if json.Unmarshal(resp.Body, &e.Body) != nil {
		e.Body.Error = strings.TrimSpace(string(resp.Body))
		if len(e.Body.Error) > 300 {
			e.Body.Error = e.Body.Error[:300]
		}
	}
	return e
}

func pathEscape(s string) string { return url.PathEscape(s) }

// --- API shapes (mirrors of internal/core and internal/store JSON) ---

type version struct {
	Flat       string          `json:"flat"`
	Number     int             `json:"number"`
	Hash       string          `json:"hash"`
	Size       int64           `json:"size"`
	Files      int             `json:"files"`
	Kind       string          `json:"kind"`
	Manifest   json.RawMessage `json:"manifest"`
	GitSHA     string          `json:"git_sha,omitempty"`
	GitDirty   bool            `json:"git_dirty"`
	Message    string          `json:"message,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	Screenshot string          `json:"screenshot,omitempty"`
	Pruned     bool            `json:"pruned"`
}

type flatView struct {
	Slug          string     `json:"slug"`
	Name          string     `json:"name"`
	Visibility    string     `json:"visibility"`
	LiveVersion   int        `json:"live_version"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	OldSlug       string     `json:"old_slug,omitempty"`
	OldSlugTill   *time.Time `json:"old_slug_until,omitempty"`
	PrivateURL    string     `json:"private_url"`
	PrivateState  string     `json:"private_state,omitempty"`
	PrivateDetail string     `json:"private_detail,omitempty"`
	PublicURL     string     `json:"public_url,omitempty"`
	PublicNotice  string     `json:"public_notice,omitempty"`
	Live          *version   `json:"live,omitempty"`
	Versions      int        `json:"versions"`
	DiskBytes     int64      `json:"disk_bytes"`
}

type health struct {
	Path     string `json:"path"`
	Status   int    `json:"status"`
	OK       bool   `json:"ok"`
	Millis   int64  `json:"millis"`
	Error    string `json:"error,omitempty"`
	BodyHead string `json:"body_head,omitempty"`
}

type deployResult struct {
	Flat     flatView `json:"flat"`
	Version  int      `json:"version"`
	Previous int      `json:"previous"`
	Health   health   `json:"health"`
	Millis   int64    `json:"millis"`
}

type problem struct {
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

type errorBody struct {
	Error    string    `json:"error"`
	Problems []problem `json:"problems,omitempty"`
	Health   *health   `json:"health,omitempty"`
}

type approval struct {
	ID          string          `json:"id"`
	Flat        string          `json:"flat"`
	Action      string          `json:"action"`
	Params      json.RawMessage `json:"params"`
	Status      string          `json:"status"`
	Via         string          `json:"via"`
	Reason      string          `json:"reason,omitempty"`
	Result      string          `json:"result,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
}

type actionResult struct {
	Status      string    `json:"status"`
	Approval    *approval `json:"approval,omitempty"`
	ApprovalURL string    `json:"approval_url,omitempty"`
	Flat        *flatView `json:"flat,omitempty"`
	Notice      string    `json:"notice,omitempty"`
	Message     string    `json:"message"`
}

type event struct {
	ID      int64           `json:"id"`
	Flat    string          `json:"flat"`
	Time    time.Time       `json:"time"`
	Level   string          `json:"level"`
	Kind    string          `json:"kind"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type secretInfo struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

type previewView struct {
	Host       string    `json:"host"`
	Flat       string    `json:"flat"`
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	LastAccess time.Time `json:"last_access"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
	State      string    `json:"state,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}
