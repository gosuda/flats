package clientstate

import (
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/buildinfo"
)

func TestCheckEvidence(t *testing.T) {
	m := NewManifest(buildinfo.Info{Version: "v1.2.3", Release: true}, 1024)
	yes, no := true, false
	tests := []struct {
		name                  string
		in                    CheckInput
		cli, guide, bootstrap string
	}{
		{name: "unknown", cli: "unknown", guide: "unknown", bootstrap: "unknown"},
		{name: "direct HTTP", in: CheckInput{CLIPresent: &no, BootstrapPresent: &no}, cli: "not_applicable", guide: "unknown", bootstrap: "not_applicable"},
		{name: "compatible different release", in: CheckInput{CLIPresent: &yes, CLIVersion: "v0.9.0", ClientContract: ContractRevision, GuideRevision: m.GuideRevision, BootstrapRevision: BootstrapRevision}, cli: "compatible", guide: "current", bootstrap: "current"},
		{name: "stale", in: CheckInput{ClientContract: 99, GuideRevision: "old", BootstrapRevision: 99}, cli: "update_required", guide: "refresh_required", bootstrap: "refresh_required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Check(m, tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if out.CLIStatus != tt.cli || out.GuideStatus != tt.guide || out.BootstrapStatus != tt.bootstrap {
				t.Fatalf("statuses = %s, %s, %s", out.CLIStatus, out.GuideStatus, out.BootstrapStatus)
			}
			if out.Skill != "" || out.CLITarget != "v1.2.3" {
				t.Fatal("check must omit skill bytes but retain release target")
			}
		})
	}
	for _, in := range []CheckInput{
		{CLIPresent: &no, CLIVersion: "v1.0.0"}, {CLIPresent: &no, ClientContract: 1},
		{BootstrapPresent: &no, BootstrapRevision: 1}, {ClientContract: -1}, {BootstrapRevision: -1},
	} {
		if _, err := Check(m, in); err == nil {
			t.Errorf("accepted conflicting input %+v", in)
		}
	}
}

func TestManifestValidation(t *testing.T) {
	m := NewManifest(buildinfo.Info{Version: "v1.2.3", Release: true}, 1024)
	if err := ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "zero contract", edit: func(m *Manifest) { m.ClientContract = 0 }},
		{name: "bad guide hash", edit: func(m *Manifest) { m.GuideRevision = "bad" }},
		{name: "changed skill", edit: func(m *Manifest) { m.Skill += "changed" }},
		{name: "wrong visible revision", edit: func(m *Manifest) { m.BootstrapRevision++ }},
		{name: "missing target", edit: func(m *Manifest) { m.CLITarget = "" }},
		{name: "different release", edit: func(m *Manifest) { m.CLITarget = "v9.0.0" }},
		{name: "dirty release target", edit: func(m *Manifest) { m.Server.Dirty = true }},
		{name: "nonrelease target", edit: func(m *Manifest) { m.Server.Release = false }},
		{name: "wrong skill", edit: func(m *Manifest) {
			m.Skill = strings.ReplaceAll(m.Skill, "name: flats-deploy", "name: another")
			m.BootstrapSHA256 = Digest(m.Skill)
		}},
		{name: "oversize", edit: func(m *Manifest) { m.Skill += strings.Repeat("x", MaxSkillBytes); m.BootstrapSHA256 = Digest(m.Skill) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copy := m
			tt.edit(&copy)
			if ValidateManifest(copy) == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	for _, build := range []buildinfo.Info{{Version: "dev"}, {Version: "abc1234"}, {Version: "v1.2.3", Release: true, Dirty: true}} {
		dev := NewManifest(build, 1024)
		if dev.CLITarget != "" {
			t.Fatal("dev build invented a release")
		}
		if err := ValidateManifest(dev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReleaseValidation(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v1.2.3-rc.1"} {
		if !ValidRelease(tag) {
			t.Errorf("rejected %q", tag)
		}
	}
	for _, tag := range []string{"latest", "v1.2.3-01", "v1.2.3/../x", "v01.2.3", "v0.0.0-sha-abcdef0", "v1.2.3+meta"} {
		if ValidRelease(tag) {
			t.Errorf("accepted %q", tag)
		}
	}
}
