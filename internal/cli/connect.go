package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The client connection is the one setting a client machine keeps: which
// Flats host the CLI and `flats mcp` talk to. It lives outside the host's
// data directory, so a machine that is both host and client never mixes it
// into the operator's config.json.

// urlFlag is the --url flag; setting it records that the flag chose the URL.
type urlFlag struct{ a *app }

func (f urlFlag) String() string {
	if f.a == nil {
		return ""
	}
	return f.a.url
}

func (f urlFlag) Set(v string) error {
	f.a.url, f.a.urlSource = v, "flag"
	return nil
}

type savedConnection struct {
	URL string `json:"url"`
}

// connectionPath is <user config dir>/flats-client/connection.json.
func (a *app) connectionPath() (string, error) {
	dir := a.env.ConfigDir
	if dir == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	return filepath.Join(dir, "flats-client", "connection.json"), nil
}

// readConnection returns the saved URL, or "" when none is saved.
func (a *app) readConnection() (string, error) {
	p, err := a.connectionPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var c savedConnection
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	u, err := normalizeHostURL(c.URL)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return u, nil
}

// defaultURL picks the server URL when --url is absent: FLATS_URL, then the
// saved connection, then the loopback default.
func (a *app) defaultURL() (string, string) {
	if u := a.env.Getenv("FLATS_URL"); u != "" {
		return u, "env"
	}
	u, err := a.readConnection()
	if err != nil {
		if !a.warned {
			a.warned = true
			fmt.Fprintf(a.errw, "flats: ignoring the saved connection: %v\n", err)
		}
		return DefaultURL, "default"
	}
	if u != "" {
		return u, "saved"
	}
	return DefaultURL, "default"
}

// normalizeHostURL accepts the console address, with or without a trailing
// slash or /mcp, and returns it without either.
func normalizeHostURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %v", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q: use http:// or https:// and a host, such as https://flats.example.ts.net", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid URL %q: give only the scheme, host and optional path", raw)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/mcp")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (a *app) connect(args []string) error {
	flags := a.flags("connect")
	forget := flags.Bool("clear", false, "forget the saved host")
	pos, err := a.parse(flags, args, 0, 1)
	if err != nil {
		return err
	}
	p, err := a.connectionPath()
	if err != nil {
		return err
	}
	switch {
	case *forget && len(pos) > 0:
		return usagef("give a URL or --clear, not both")
	case *forget:
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if a.urlSource != "flag" {
			a.url, a.urlSource = a.defaultURL()
		}
		return a.reportConnection(p, "Forgot the saved host.")
	case len(pos) == 0:
		return a.reportConnection(p, "")
	}

	base, err := normalizeHostURL(pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	// Save only an address that answers as a Flats host: a catch-all page or
	// a login redirect can also answer 200.
	c := &client{base: base, hc: a.env.HTTP}
	var st struct {
		OK             bool  `json:"ok"`
		UploadMaxBytes int64 `json:"upload_max_bytes"`
	}
	if _, err := c.call(a.ctx, http.MethodGet, "/api/status", nil, nil, &st); err != nil {
		var ae *apiError
		var ue *unreachableError
		switch {
		case errors.As(err, &ae):
			return fmt.Errorf("%s answered %d to /api/status; is it a Flats console address? (nothing saved)", base, ae.Status)
		case errors.As(err, &ue), a.ctx.Err() != nil:
			return fmt.Errorf("%w\n(nothing saved)", err)
		}
		return fmt.Errorf("%s did not answer /api/status as a Flats host; is it the console address? (nothing saved)", base)
	}
	if !st.OK || st.UploadMaxBytes <= 0 {
		return fmt.Errorf("%s did not answer /api/status as a Flats host; is it the console address? (nothing saved)", base)
	}
	b, _ := json.MarshalIndent(savedConnection{URL: base}, "", "  ")
	if err := writeFileAtomic(p, append(b, '\n')); err != nil {
		return err
	}
	if a.urlSource != "flag" {
		a.url, a.urlSource = a.defaultURL()
	}
	return a.reportConnection(p, "Saved "+base+".")
}

func (a *app) reportConnection(path, note string) error {
	saved, _ := a.readConnection()
	if a.jsonOut {
		a.writeJSON(map[string]string{"url": a.url, "source": a.urlSource, "saved": saved, "file": path})
		return nil
	}
	if note != "" {
		fmt.Fprintln(a.out, note)
	}
	fmt.Fprintf(a.out, "Flats host: %s (%s)\n", a.url, map[string]string{
		"flag": "from --url", "env": "from FLATS_URL", "saved": "saved in " + path, "default": "loopback default",
	}[a.urlSource])
	if a.urlSource == "env" && saved != "" {
		fmt.Fprintf(a.out, "FLATS_URL overrides the saved host %s.\n", saved)
	}
	if note != "" {
		fmt.Fprintln(a.out, "The CLI and `flats mcp` use it from now on; restart or reconnect agent tools that already started `flats mcp`.")
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".connection-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
