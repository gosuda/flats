// Package clientstate describes compatibility between a Flats host and its clients.
package clientstate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/plugins"
)

const ContractRevision = 2
const BootstrapRevision = 4
const MaxSkillBytes = 32 << 10

type Manifest struct {
	Server            buildinfo.Info `json:"server"`
	ClientContract    int            `json:"client_contract"`
	GuideRevision     string         `json:"guide_revision"`
	BootstrapRevision int            `json:"bootstrap_revision"`
	BootstrapSHA256   string         `json:"bootstrap_sha256"`
	CLITarget         string         `json:"cli_target,omitempty"`
	Skill             string         `json:"skill,omitempty"`
	ServerURL         string         `json:"server_url,omitempty"`
}

type CheckInput struct {
	CLIPresent        *bool  `json:"cli_present,omitempty"`
	CLIVersion        string `json:"cli_version,omitempty"`
	ClientContract    int    `json:"client_contract,omitempty"`
	GuideRevision     string `json:"guide_revision,omitempty"`
	BootstrapPresent  *bool  `json:"bootstrap_present,omitempty"`
	BootstrapRevision int    `json:"bootstrap_revision,omitempty"`
}

type CheckResult struct {
	Manifest
	CLIStatus       string `json:"cli_status"`
	GuideStatus     string `json:"guide_status"`
	BootstrapStatus string `json:"bootstrap_status"`
}

// NewManifest returns this build's client resources, without an inferred address.
func NewManifest(build buildinfo.Info, uploadLimit int64) Manifest {
	m := Manifest{
		Server: build, ClientContract: ContractRevision,
		GuideRevision:     runtimeref.GuideRevision(uploadLimit),
		BootstrapRevision: BootstrapRevision, BootstrapSHA256: Digest(plugins.Skill), Skill: plugins.Skill,
	}
	if build.Release && !build.Dirty && ValidRelease(build.Version) {
		m.CLITarget = build.Version
	}
	return m
}

var releasePattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// ValidRelease accepts release tags without path components or build metadata.
func ValidRelease(version string) bool {
	if !releasePattern.MatchString(version) || strings.HasPrefix(version, "v0.0.0-") {
		return false
	}
	_, prerelease, ok := strings.Cut(version, "-")
	if !ok {
		return true
	}
	for _, part := range strings.Split(prerelease, ".") {
		numeric := strings.Trim(part, "0123456789") == ""
		if numeric && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

// Digest is the lower-case SHA-256 of exact content bytes.
func Digest(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content))) }

func validDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && value == strings.ToLower(value)
}

// SkillRevision reads the visible revision of a bounded Flats bootstrap.
func SkillRevision(skill string) (int, error) {
	if len(skill) > MaxSkillBytes || !strings.HasPrefix(skill, "---\n") {
		return 0, fmt.Errorf("skill must be a Flats SKILL.md of at most %d bytes", MaxSkillBytes)
	}
	front, body, ok := strings.Cut(strings.TrimPrefix(skill, "---\n"), "\n---\n")
	if !ok {
		return 0, fmt.Errorf("skill frontmatter is missing")
	}
	found := 0
	for _, line := range strings.Split(front, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "name:") {
			if strings.TrimSpace(line) != "name: flats-deploy" {
				return 0, fmt.Errorf("skill frontmatter must name flats-deploy")
			}
			found++
		}
	}
	if found != 1 {
		return 0, fmt.Errorf("skill frontmatter must name flats-deploy")
	}
	revision := 0
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(line, "Flats bootstrap revision: "); ok {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n <= 0 || revision != 0 {
				return 0, fmt.Errorf("invalid bootstrap revision")
			}
			revision = n
		}
	}
	if revision == 0 {
		return 0, fmt.Errorf("skill has no visible bootstrap revision")
	}
	return revision, nil
}

// ValidateManifest rejects partial or internally inconsistent remote manifests.
func ValidateManifest(m Manifest) error {
	if m.Server.Version == "" {
		return fmt.Errorf("manifest server build is missing")
	}
	if m.ClientContract <= 0 || m.BootstrapRevision <= 0 {
		return fmt.Errorf("manifest revisions must be positive")
	}
	if !validDigest(m.GuideRevision) || !validDigest(m.BootstrapSHA256) {
		return fmt.Errorf("manifest contains an invalid SHA-256")
	}
	revision, err := SkillRevision(m.Skill)
	if err != nil {
		return err
	}
	if revision != m.BootstrapRevision || Digest(m.Skill) != m.BootstrapSHA256 {
		return fmt.Errorf("manifest skill revision or digest does not match its content")
	}
	release := m.Server.Release && !m.Server.Dirty
	if release {
		if !ValidRelease(m.Server.Version) || m.CLITarget != m.Server.Version {
			return fmt.Errorf("manifest release target does not match the server")
		}
	} else if m.CLITarget != "" {
		return fmt.Errorf("development server cannot name a release target")
	}
	return nil
}

// Check compares only evidence provided by the caller; absent data stays unknown.
func Check(m Manifest, in CheckInput) (CheckResult, error) {
	if err := ValidateManifest(m); err != nil {
		return CheckResult{}, err
	}
	if in.ClientContract < 0 || in.BootstrapRevision < 0 {
		return CheckResult{}, fmt.Errorf("revisions cannot be negative")
	}
	noCLI := in.CLIPresent != nil && !*in.CLIPresent
	noBootstrap := in.BootstrapPresent != nil && !*in.BootstrapPresent
	if noCLI && (in.CLIVersion != "" || in.ClientContract != 0) {
		return CheckResult{}, fmt.Errorf("cli_present false conflicts with CLI identity")
	}
	if noBootstrap && in.BootstrapRevision != 0 {
		return CheckResult{}, fmt.Errorf("bootstrap_present false conflicts with bootstrap revision")
	}
	m.Skill = ""
	out := CheckResult{Manifest: m, CLIStatus: "unknown", GuideStatus: "unknown", BootstrapStatus: "unknown"}
	switch {
	case noCLI:
		out.CLIStatus = "not_applicable"
	case in.ClientContract == m.ClientContract:
		out.CLIStatus = "compatible"
	case in.ClientContract > 0:
		out.CLIStatus = "update_required"
	}
	if in.GuideRevision != "" {
		out.GuideStatus = "refresh_required"
		if in.GuideRevision == m.GuideRevision {
			out.GuideStatus = "current"
		}
	}
	switch {
	case noBootstrap:
		out.BootstrapStatus = "not_applicable"
	case in.BootstrapRevision == m.BootstrapRevision:
		out.BootstrapStatus = "current"
	case in.BootstrapRevision > 0:
		out.BootstrapStatus = "refresh_required"
	}
	return out, nil
}
