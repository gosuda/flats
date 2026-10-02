package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// --- deploy ---

func (a *app) deploy(args []string) error {
	fs := a.flags("deploy")
	slug := fs.String("flat", "", "flat slug (created on first deploy)")
	saveOnly := fs.Bool("save-only", false, "save the version without deploying it")
	message := fs.String("message", "", "note stored with the version")
	n := fs.Int("version", 0, "deploy an already saved version instead of uploading")
	pos, err := a.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if *slug == "" {
		return usagef("--flat is required")
	}
	if *n > 0 {
		if len(pos) > 0 || *saveOnly || *message != "" {
			return usagef("--version deploys a saved version; do not pass a directory, --save-only or --message")
		}
		return a.deployExisting(*slug, *n)
	}
	if len(pos) == 0 {
		return usagef("missing build directory")
	}
	return a.upload(pos[0], *slug, *message, !*saveOnly)
}

func (a *app) upload(src, slug, message string, deploy bool) error {
	start := time.Now()
	c := a.client()

	// Ask the server first: this fails fast when it is down and gives the
	// size limit before a large directory is packed.
	var st struct {
		UploadMaxBytes int64 `json:"upload_max_bytes"`
	}
	if _, err := c.call(a.ctx, http.MethodGet, "/api/status", nil, nil, &st); err != nil {
		return err
	}

	path, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	var body []byte
	var ps packStats
	gitDir, ctype := path, "application/gzip"
	if info.IsDir() {
		body, ps, err = packDir(path, st.UploadMaxBytes)
		if err != nil {
			return err
		}
	} else {
		// An archive (tar, tar.gz or zip) is sent as is; the server sniffs
		// its format. Refuse files the server would reject anyway (it reads
		// at most the limit plus a quarter plus 1 MiB) before loading them.
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is neither a directory nor an archive file", src)
		}
		if lim := st.UploadMaxBytes; lim > 0 && info.Size() > lim+lim/4+1<<20 {
			return fmt.Errorf("%s is %s, larger than the server's %s upload limit", src, humanBytes(info.Size()), humanBytes(lim))
		}
		gitDir, ctype = filepath.Dir(path), "application/octet-stream"
		body, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	packed := time.Since(start)

	q := url.Values{}
	gi, haveGit := detectGit(a.ctx, gitDir)
	if haveGit {
		q.Set("git_sha", gi.SHA)
		q.Set("git_dirty", strconv.FormatBool(gi.Dirty))
	}
	if message != "" {
		q.Set("message", message)
	}
	if deploy {
		q.Set("deploy", "1")
	}
	sent := time.Now()
	resp, err := c.do(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/versions", q, bytes.NewReader(body), ctype)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
	}
	var out struct {
		Version     *version      `json:"version"`
		Deploy      *deployResult `json:"deploy"`
		DeployError *errorBody    `json:"deploy_error"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	if resp.Status >= 300 && out.DeployError == nil {
		if a.jsonOut {
			return exitCode(ExitError)
		}
		return newAPIError(resp)
	}
	if a.jsonOut {
		if out.DeployError != nil {
			return exitCode(ExitError)
		}
		return nil
	}

	w := a.out
	if v := out.Version; v != nil {
		fmt.Fprintf(w, "Saved version %d of %s: %d files, %s%s\n", v.Number, slug, v.Files, humanBytes(v.Size), gitLabel(v.GitSHA, v.GitDirty))
	}
	if info.IsDir() && ps.Skipped > 0 {
		fmt.Fprintf(w, "  skipped %d entries (.git, node_modules, .DS_Store, ...)\n", ps.Skipped)
	}
	if out.DeployError != nil {
		fmt.Fprintf(a.errw, "error: %s\n", out.DeployError.Error)
		if h := out.DeployError.Health; h != nil {
			printHealth(a.errw, *h)
		}
		if out.Version != nil {
			fmt.Fprintf(a.errw, "The version is saved; fix the problem and deploy again, or deploy it later with `flats deploy --flat %s --version %d`.\n", slug, out.Version.Number)
		}
		return exitCode(ExitError)
	}
	if d := out.Deploy; d != nil {
		printDeploy(w, *d)
	} else if out.Version != nil {
		fmt.Fprintf(w, "Not deployed (--save-only). Deploy it with `flats deploy --flat %s --version %d`, or preview it with `flats preview %s --version %d`.\n",
			slug, out.Version.Number, slug, out.Version.Number)
	}
	total := time.Since(start)
	fmt.Fprintf(w, "Took %s (pack %s, upload %s", roundDur(total), roundDur(packed), humanBytes(int64(len(body))))
	if out.Deploy != nil {
		fmt.Fprintf(w, ", deploy %s", roundDur(time.Duration(out.Deploy.Millis)*time.Millisecond))
	}
	fmt.Fprintf(w, ", server round trip %s)\n", roundDur(time.Since(sent)))
	return nil
}

func (a *app) deployExisting(slug string, n int) error {
	var res deployResult
	resp, err := a.client().call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/deploy", nil, map[string]int{"version": n}, &res)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	printDeploy(a.out, res)
	return nil
}

func printDeploy(w io.Writer, d deployResult) {
	if d.Previous > 0 {
		fmt.Fprintf(w, "Version %d is live (was %d).\n", d.Version, d.Previous)
	} else {
		fmt.Fprintf(w, "Version %d is live.\n", d.Version)
	}
	printHealth(w, d.Health)
	printURLs(w, d.Flat)
}

func printHealth(w io.Writer, h health) {
	status := "failed"
	if h.OK {
		status = "ok"
	}
	line := fmt.Sprintf("  health: GET %s", h.Path)
	if h.Status != 0 {
		line += fmt.Sprintf(" -> %d", h.Status)
	}
	line += fmt.Sprintf(" in %dms (%s)", h.Millis, status)
	if h.Error != "" {
		line += ": " + h.Error
	}
	fmt.Fprintln(w, line)
	if !h.OK && h.BodyHead != "" {
		fmt.Fprintf(w, "  response body: %s\n", oneLine(h.BodyHead))
	}
}

func printURLs(w io.Writer, f flatView) {
	if f.PrivateURL != "" {
		fmt.Fprintf(w, "  private: %s\n", f.PrivateURL)
	}
	if f.PublicURL != "" {
		fmt.Fprintf(w, "  public:  %s\n", f.PublicURL)
	}
	if f.PublicNotice != "" {
		fmt.Fprintf(w, "  note: %s\n", f.PublicNotice)
	}
}

// --- read commands ---

func (a *app) list(args []string) error {
	fs := a.flags("list")
	if _, err := a.parse(fs, args, 0, 0); err != nil {
		return err
	}
	var out struct {
		Flats []flatView `json:"flats"`
	}
	resp, err := a.client().call(a.ctx, http.MethodGet, "/api/flats", nil, nil, &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	if len(out.Flats) == 0 {
		fmt.Fprintln(a.out, "No flats yet. Deploy one with `flats deploy <dir> --flat <slug>`.")
		return nil
	}
	tw := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tVISIBILITY\tLIVE\tVERSIONS\tURL")
	for _, f := range out.Flats {
		u := f.PrivateURL
		if f.PublicURL != "" {
			u = f.PublicURL
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", f.Slug, f.Visibility, liveLabel(f.LiveVersion), f.Versions, u)
	}
	return tw.Flush()
}

func (a *app) info(args []string) error {
	fs := a.flags("info")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	var f flatView
	resp, err := a.client().call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(pos[0]), nil, nil, &f)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	w := a.out
	fmt.Fprintf(w, "%s (%s)\n", f.Slug, f.Name)
	fmt.Fprintf(w, "  visibility: %s\n", f.Visibility)
	if lv := f.Live; lv != nil {
		fmt.Fprintf(w, "  live:       version %d, %s, %d files, %s%s\n", lv.Number, lv.Kind, lv.Files, humanBytes(lv.Size), gitLabel(lv.GitSHA, lv.GitDirty))
	} else {
		fmt.Fprintf(w, "  live:       not deployed\n")
	}
	fmt.Fprintf(w, "  versions:   %d (%s on disk)\n", f.Versions, humanBytes(f.DiskBytes))
	if f.OldSlug != "" && f.OldSlugTill != nil {
		fmt.Fprintf(w, "  old slug:   %s redirects until %s\n", f.OldSlug, f.OldSlugTill.Local().Format(timeFmt))
	}
	printURLs(w, f)
	return nil
}

func (a *app) versions(args []string) error {
	fs := a.flags("versions")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c := a.client()
	var out struct {
		Versions []version `json:"versions"`
	}
	resp, err := c.call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(pos[0])+"/versions", nil, nil, &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	var f flatView
	if _, err := c.call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(pos[0]), nil, nil, &f); err != nil {
		return err
	}
	if len(out.Versions) == 0 {
		fmt.Fprintln(a.out, "No versions saved yet.")
		return nil
	}
	tw := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\t\tKIND\tFILES\tSIZE\tGIT\tCREATED\tMESSAGE")
	for _, v := range out.Versions {
		mark := ""
		switch {
		case v.Number == f.LiveVersion:
			mark = "live"
		case v.Pruned:
			mark = "pruned"
		}
		g := shortSHA(v.GitSHA)
		if g != "" && v.GitDirty {
			g += "+dirty"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", v.Number, mark, v.Kind, v.Files, humanBytes(v.Size), dash(g), v.CreatedAt.Local().Format(timeFmt), oneLine(v.Message))
	}
	return tw.Flush()
}

// --- changes ---

func (a *app) rollback(args []string) error {
	fs := a.flags("rollback")
	to := fs.Int("to", 0, "version to make live (default: the one live before the current)")
	restore := fs.Bool("restore-data", false, "also restore the database snapshot taken before the current version was deployed")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *to < 0 {
		return usagef("--to must be a positive version number")
	}
	var res deployResult
	resp, err := a.client().call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(pos[0])+"/rollback", nil, map[string]any{"version": *to, "restore_data": *restore}, &res)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	printDeploy(a.out, res)
	return nil
}

func (a *app) preview(args []string) error {
	fs := a.flags("preview")
	n := fs.Int("version", 0, "version to preview (default: newest)")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c := a.client()
	slug := pos[0]
	if *n <= 0 {
		var out struct {
			Versions []version `json:"versions"`
		}
		if _, err := c.call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(slug)+"/versions", nil, nil, &out); err != nil {
			return err
		}
		for _, v := range out.Versions {
			if v.Number > *n {
				*n = v.Number
			}
		}
		if *n == 0 {
			return fmt.Errorf("%s has no saved versions; save one with `flats deploy <dir> --flat %s --save-only`", slug, slug)
		}
	}
	var p previewView
	resp, err := c.call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/previews", nil, map[string]int{"version": *n}, &p)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	fmt.Fprintf(a.out, "Preview of %s version %d: %s\n", slug, p.Version, p.URL)
	fmt.Fprintf(a.out, "  private to your tailnet; closes on the next deploy or after 24h without visits (now: %s)\n", p.ExpiresAt.Local().Format(timeFmt))
	return nil
}

func (a *app) visibility(args []string) error {
	fs := a.flags("visibility")
	reason := fs.String("reason", "", "why (shown to the operator when approval is needed)")
	pos, err := a.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	switch pos[1] {
	case "private", "public-listed", "public-unlisted":
	default:
		return usagef("visibility must be private, public-listed or public-unlisted")
	}
	var res actionResult
	resp, err := a.client().call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(pos[0])+"/visibility", nil,
		map[string]string{"visibility": pos[1], "reason": *reason}, &res)
	if err != nil {
		return err
	}
	return a.action(resp, res, pos[1])
}

func (a *app) delete(args []string) error {
	fs := a.flags("delete")
	reason := fs.String("reason", "", "why (shown to the operator)")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	var q url.Values
	if *reason != "" {
		q = url.Values{"reason": {*reason}}
	}
	var res actionResult
	resp, err := a.client().call(a.ctx, http.MethodDelete, "/api/flats/"+pathEscape(pos[0]), q, nil, &res)
	if err != nil {
		return err
	}
	return a.action(resp, res, "")
}

// unlistedNotice mirrors core.UnlistedNotice (a test keeps them equal). The
// server attaches it to applied changes; the CLI also shows it when a
// request for unlisted exposure is still waiting for approval.
const unlistedNotice = "Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it."

// action prints a result that may be waiting for approval (exit 3).
func (a *app) action(resp response, res actionResult, requested string) error {
	pending := res.Status == "pending_approval"
	if a.jsonOut {
		a.emitRaw(resp)
	} else {
		if res.Message != "" {
			fmt.Fprintln(a.out, res.Message)
		}
		if pending {
			fmt.Fprintf(a.out, "Approval needed: %s\n", res.ApprovalURL)
		}
		if res.Flat != nil {
			fmt.Fprintf(a.out, "%s is %s\n", res.Flat.Slug, res.Flat.Visibility)
			if res.Flat.PublicURL != "" {
				fmt.Fprintf(a.out, "  public: %s\n", res.Flat.PublicURL)
			}
		}
		switch {
		case res.Notice != "":
			fmt.Fprintln(a.out, res.Notice)
		case pending && requested == "public-unlisted":
			fmt.Fprintln(a.out, unlistedNotice)
		}
	}
	if pending {
		return exitCode(ExitPending)
	}
	return nil
}

func (a *app) rename(args []string) error {
	fs := a.flags("rename")
	pos, err := a.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	var f flatView
	resp, err := a.client().call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(pos[0])+"/rename", nil, map[string]string{"slug": pos[1]}, &f)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	fmt.Fprintf(a.out, "Renamed %s to %s.\n", pos[0], f.Slug)
	if f.OldSlug != "" && f.OldSlugTill != nil {
		fmt.Fprintf(a.out, "  the old address redirects until %s\n", f.OldSlugTill.Local().Format(timeFmt))
	}
	printURLs(a.out, f)
	return nil
}

// --- logs ---

func (a *app) logs(args []string) error {
	fs := a.flags("logs")
	follow := fs.Bool("follow", false, "keep printing new events")
	fs.BoolVar(follow, "f", false, "shorthand for --follow")
	kind := fs.String("kind", "", "only events of this kind (deploy, health, runtime, visibility, ...)")
	limit := fs.Int("limit", 100, "number of recent events (1-1000)")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	// The server silently replaces an out-of-range limit with its default.
	if *limit < 1 || *limit > 1000 {
		return usagef("--limit must be between 1 and 1000")
	}
	c := a.client()
	path := "/api/flats/" + pathEscape(pos[0]) + "/logs"
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *kind != "" {
		q.Set("kind", *kind)
	}
	var out struct {
		Events []event `json:"events"`
	}
	resp, err := c.call(a.ctx, http.MethodGet, path, q, nil, &out)
	if err != nil {
		return err
	}
	if a.jsonOut && !*follow {
		a.emitRaw(resp)
		return nil
	}
	var last int64
	show := func(evs []event) {
		for _, e := range evs {
			if e.ID > last {
				last = e.ID
			}
			if a.jsonOut {
				b, _ := json.Marshal(e)
				fmt.Fprintf(a.out, "%s\n", b)
				continue
			}
			fmt.Fprintf(a.out, "%s  %-5s  %-10s  %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Level, e.Kind, e.Message)
		}
	}
	show(out.Events)
	if !*follow {
		return nil
	}
	interval := a.env.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return nil
		case <-t.C:
		}
		q.Set("after", strconv.FormatInt(last, 10))
		q.Set("limit", "1000")
		out.Events = nil
		if _, err := c.call(a.ctx, http.MethodGet, path, q, nil, &out); err != nil {
			if a.ctx.Err() != nil {
				return nil
			}
			return err
		}
		show(out.Events)
	}
}

// --- secrets ---

func (a *app) secret(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (set, rm or ls)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("secret")
	switch sub {
	case "set":
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		value, err := a.readSecret(pos[1])
		if err != nil {
			return err
		}
		resp, err := a.client().call(a.ctx, http.MethodPut, "/api/flats/"+pathEscape(pos[0])+"/secrets/"+pathEscape(pos[1]), nil, map[string]string{"value": value}, nil)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emitRaw(resp)
			return nil
		}
		fmt.Fprintf(a.out, "Stored %s for %s. It applies on the next deploy.\n", pos[1], pos[0])
		return nil
	case "rm", "delete":
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		resp, err := a.client().call(a.ctx, http.MethodDelete, "/api/flats/"+pathEscape(pos[0])+"/secrets/"+pathEscape(pos[1]), nil, nil, nil)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emitRaw(resp)
			return nil
		}
		fmt.Fprintf(a.out, "Deleted %s from %s. It is removed on the next deploy.\n", pos[1], pos[0])
		return nil
	case "ls", "list":
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		var out struct {
			Secrets []secretInfo `json:"secrets"`
		}
		resp, err := a.client().call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(pos[0])+"/secrets", nil, nil, &out)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emitRaw(resp)
			return nil
		}
		if len(out.Secrets) == 0 {
			fmt.Fprintln(a.out, "No secrets.")
			return nil
		}
		tw := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tUPDATED")
		for _, s := range out.Secrets {
			fmt.Fprintf(tw, "%s\t%s\n", s.Name, s.UpdatedAt.Local().Format(timeFmt))
		}
		return tw.Flush()
	}
	return usagef("unknown secret subcommand %q (use set, rm or ls)", sub)
}

// readSecret reads a value from stdin: without echo from a terminal, or the
// whole input (minus one trailing newline) from a pipe.
func (a *app) readSecret(name string) (string, error) {
	if f, ok := a.env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		saved, err := term.GetState(fd)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(a.errw, "Value for %s (input hidden): ", name)
		// main catches SIGINT, so Ctrl-C would not interrupt the read itself:
		// wait for it or for cancellation, and restore echo on the way out.
		type read struct {
			b   []byte
			err error
		}
		ch := make(chan read, 1)
		go func() {
			b, err := term.ReadPassword(fd)
			ch <- read{b, err}
		}()
		var r read
		select {
		case r = <-ch:
		case <-a.ctx.Done():
			_ = term.Restore(fd, saved)
			fmt.Fprintln(a.errw)
			return "", a.ctx.Err()
		}
		fmt.Fprintln(a.errw)
		if r.err != nil {
			return "", r.err
		}
		if len(r.b) == 0 {
			return "", errors.New("empty value; nothing stored")
		}
		return string(r.b), nil
	}
	b, err := io.ReadAll(io.LimitReader(a.env.Stdin, 1<<20))
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if s == "" {
		return "", errors.New("empty value on stdin; pipe the value in, e.g. `printf %s \"$TOKEN\" | flats secret set <slug> <NAME>`")
	}
	return s, nil
}

// --- approvals ---

func (a *app) approvals(args []string) error {
	fs := a.flags("approvals")
	status := fs.String("status", "", "filter: pending, approved, rejected or failed")
	if _, err := a.parse(fs, args, 0, 0); err != nil {
		return err
	}
	var q url.Values
	if *status != "" {
		q = url.Values{"status": {*status}}
	}
	var out struct {
		Approvals []approval `json:"approvals"`
	}
	resp, err := a.client().call(a.ctx, http.MethodGet, "/api/approvals", q, nil, &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	if len(out.Approvals) == 0 {
		fmt.Fprintln(a.out, "No approval requests.")
		return nil
	}
	tw := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFLAT\tACTION\tSTATUS\tVIA\tREQUESTED\tREASON")
	for _, ap := range out.Approvals {
		action := ap.Action
		var p map[string]string
		if json.Unmarshal(ap.Params, &p) == nil && p["visibility"] != "" {
			action += " " + p["visibility"]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ap.ID, ap.Flat, action, ap.Status, ap.Via, ap.RequestedAt.Local().Format(timeFmt), oneLine(ap.Reason))
	}
	return tw.Flush()
}

// --- formatting ---

const timeFmt = "2006-01-02 15:04"

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func gitLabel(sha string, dirty bool) string {
	if sha == "" {
		return ""
	}
	s := ", git " + shortSHA(sha)
	if dirty {
		s += " (dirty)"
	}
	return s
}

func liveLabel(n int) string {
	if n == 0 {
		return "-"
	}
	return "v" + strconv.Itoa(n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:117]) + "..."
	}
	return s
}

func roundDur(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(10 * time.Millisecond)
}
