package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/selfupdate"
)

func coded(code string, err error) error { return &selfupdate.Error{Code: code, Err: err} }
func (a *app) resourceError(err error) error {
	if err == nil {
		return nil
	}
	var exit exitCode
	if errors.As(err, &exit) {
		return err
	}
	code := selfupdate.Code(err)
	status := ExitError
	var usage usageError
	if errors.As(err, &usage) {
		code = "invalid_target"
		status = ExitUsage
	}
	if a.jsonOut {
		a.writeJSON(map[string]any{"error": map[string]string{"code": code, "message": err.Error()}})
		return exitCode(status)
	}
	return err
}
func (a *app) executable() (string, error) {
	fn := a.env.Executable
	if fn == nil {
		fn = os.Executable
	}
	p, e := fn()
	if e != nil {
		return "", e
	}
	return filepath.Abs(p)
}
func (a *app) clientManifest() (clientstate.Manifest, error) {
	var m clientstate.Manifest
	u, e := normalizeHostURL(a.url)
	if e != nil {
		return m, coded("invalid_target", e)
	}
	a.url = u
	reply, e := a.client().do(a.ctx, http.MethodGet, "/api/client", nil, nil, "")
	if e != nil {
		return m, coded("transport", e)
	}
	if reply.Status != http.StatusOK {
		return m, coded("transport", newAPIError(reply))
	}
	if e = json.Unmarshal(reply.Body, &m); e != nil {
		return m, coded("invalid_manifest", e)
	}
	if e = clientstate.ValidateManifest(m); e != nil {
		return m, coded("invalid_manifest", e)
	}
	m.ServerURL = a.url
	return m, nil
}
func (a *app) clientCommand(args []string) error { return a.resourceError(a.checkClient(args)) }
func (a *app) checkClient(args []string) error {
	fs := a.flags("client")
	skill := fs.String("skill-file", "", "explicit installed skill to inspect (never discovers agent directories)")
	pos, e := a.parse(fs, args, 1, 1)
	if e != nil {
		return e
	}
	if pos[0] != "check" {
		return usagef("expected client check")
	}
	m, e := a.clientManifest()
	if e != nil {
		return e
	}
	exe, e := a.executable()
	if e != nil {
		return e
	}
	presentCLI := true
	in := clientstate.CheckInput{CLIPresent: &presentCLI, CLIVersion: buildinfo.Get().Version, ClientContract: clientstate.ContractRevision}
	var exists *bool
	hash := ""
	skillPath := ""
	if *skill != "" {
		skillPath, e = filepath.Abs(*skill)
		if e != nil {
			return e
		}
		data, err := readLocalSkill(skillPath)
		present := err == nil
		exists = &present
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if present {
			hash = selfupdate.Digest(data)
			in.BootstrapRevision, _ = clientstate.SkillRevision(string(data))
		}
	}
	result, e := clientstate.Check(m, in)
	if e != nil {
		return coded("invalid_manifest", e)
	}
	if exists != nil && !*exists {
		result.BootstrapStatus = "refresh_required"
	}
	out := struct {
		clientstate.CheckResult
		Executable  string `json:"executable"`
		SkillFile   string `json:"skill_file,omitempty"`
		SkillSHA256 string `json:"skill_sha256,omitempty"`
		SkillExists *bool  `json:"skill_exists,omitempty"`
	}{result, exe, skillPath, hash, exists}
	if a.jsonOut {
		a.writeJSON(out)
	} else {
		fmt.Fprintf(a.out, "Server: %s (%s)\nCLI: %s, contract %d — %s\nExecutable: %s\nGuide: %s\nBootstrap: %s\n", a.url, m.Server.Version, buildinfo.Get().Version, clientstate.ContractRevision, result.CLIStatus, exe, result.GuideStatus, result.BootstrapStatus)
	}
	return nil
}

type updateResult struct {
	Status                string `json:"status"`
	Path                  string `json:"path"`
	ServerURL             string `json:"server_url,omitempty"`
	Version               string `json:"version"`
	ClientContract        int    `json:"client_contract"`
	PreviousVersion       string `json:"previous_version"`
	Direction             string `json:"direction"`
	ReconnectRequired     bool   `json:"reconnect_required"`
	CompatibilityAssessed bool   `json:"compatibility_assessed"`
	Notice                string `json:"notice"`
}

const updateNotice = "Do not share this CLI path with a running foreground/custom Flats server: new workers can use the replaced binary immediately. Remote server was not updated."

func (a *app) updateCommand(args []string) error { return a.resourceError(a.updateClient(args)) }
func (a *app) updateClient(args []string) error {
	fs := a.flags("update")
	version := fs.String("version", "", "explicit official release (server compatibility must be checked afterward)")
	if _, e := a.parse(fs, args, 0, 0); e != nil {
		return e
	}
	exe, e := a.executable()
	if e != nil {
		return e
	}
	current := buildinfo.Get()
	result := updateResult{Status: "current", Path: exe, Version: current.Version, PreviousVersion: current.Version, ClientContract: clientstate.ContractRevision, Direction: "same", Notice: updateNotice}
	opts := selfupdate.Options{Path: exe, Version: *version, GOOS: a.goos(), GOARCH: runtime.GOARCH, Home: a.env.Home, ConfigDir: a.env.ConfigDir, Transport: a.env.ReleaseTransport, Probe: a.env.CandidateProbe}
	if *version == "" {
		m, e := a.clientManifest()
		if e != nil {
			return e
		}
		result.ServerURL = a.url
		result.CompatibilityAssessed = true
		if m.ClientContract == clientstate.ContractRevision {
			return a.printUpdate(result)
		}
		if m.CLITarget == "" {
			return coded("dev_target", fmt.Errorf("server %s is a development/unreleased build with no downloadable CLI target", a.url))
		}
		opts.Version = m.CLITarget
		opts.RequiredContract = m.ClientContract
		opts.Recheck = func() error {
			next, e := a.clientManifest()
			if e != nil {
				return e
			}
			if next.CLITarget != m.CLITarget || next.ClientContract != m.ClientContract {
				return coded("server_changed", fmt.Errorf("server target or contract changed; check again"))
			}
			return nil
		}
	}
	id, p, e := selfupdate.Update(a.ctx, opts)
	if e != nil {
		return e
	}
	result.Status = "updated"
	result.Path = p
	result.Version = id.Version
	result.ClientContract = id.ClientContract
	result.Direction = selfupdate.Direction(current.Version, id.Version)
	result.ReconnectRequired = true
	return a.printUpdate(result)
}
func (a *app) printUpdate(result updateResult) error {
	if a.jsonOut {
		a.writeJSON(result)
	} else {
		fmt.Fprintf(a.out, "CLI %s: %s at %s (%s)\n%s\n", result.Status, result.Version, result.Path, result.Direction, result.Notice)
		if result.ReconnectRequired {
			fmt.Fprintln(a.out, "Reconnect active MCP bridges, then check compatibility on the same server.")
		}
	}
	return nil
}
func (a *app) skillCommand(args []string) error { return a.resourceError(a.syncSkill(args)) }
func (a *app) syncSkill(args []string) error {
	fs := a.flags("skill")
	output := fs.String("output", "", "standalone SKILL.md only; update managed plugins with their plugin manager")
	expected := fs.String("expected-sha256", "", "exact current file hash required to replace differing content")
	pos, e := a.parse(fs, args, 1, 1)
	if e != nil {
		return e
	}
	if pos[0] != "sync" || *output == "" {
		return usagef("expected skill sync --output <SKILL.md path>")
	}
	if filepath.Base(*output) != "SKILL.md" {
		return usagef("output filename must be SKILL.md")
	}
	m, e := a.clientManifest()
	if e != nil {
		return e
	}
	changed, p, e := selfupdate.SyncSkill(a.ctx, *output, []byte(m.Skill), strings.ToLower(*expected), a.env.ConfigDir, func() error {
		next, e := a.clientManifest()
		if e != nil {
			return e
		}
		if next.BootstrapRevision != m.BootstrapRevision || next.BootstrapSHA256 != m.BootstrapSHA256 || next.ClientContract != m.ClientContract || next.CLITarget != m.CLITarget {
			return coded("server_changed", fmt.Errorf("server skill, target or contract changed; check again"))
		}
		return nil
	})
	if e != nil {
		return e
	}
	status := "current"
	if changed {
		status = "updated"
	}
	result := struct {
		Status            string `json:"status"`
		Path              string `json:"path"`
		ServerURL         string `json:"server_url"`
		BootstrapRevision int    `json:"bootstrap_revision"`
		BootstrapSHA256   string `json:"bootstrap_sha256"`
		ReloadRequired    bool   `json:"reload_required"`
	}{status, p, a.url, m.BootstrapRevision, m.BootstrapSHA256, changed}
	if a.jsonOut {
		a.writeJSON(result)
	} else {
		fmt.Fprintf(a.out, "Standalone skill %s: %s\nServer: %s\nBootstrap revision: %d\n", status, p, a.url, m.BootstrapRevision)
		if changed {
			fmt.Fprintln(a.out, "Read the new skill for this task; reload the agent skill catalog if supported.")
		}
	}
	return nil
}

func readLocalSkill(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("installed skill is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, clientstate.MaxSkillBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > clientstate.MaxSkillBytes {
		return nil, fmt.Errorf("installed skill exceeds %d bytes", clientstate.MaxSkillBytes)
	}
	return b, nil
}
