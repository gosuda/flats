package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/buildinfo"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func archiveFixture(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if e := tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Typeflag == tar.TypeReg {
			if _, e := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func setup(t *testing.T) (Options, []byte) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "flats")
	if e = os.WriteFile(p, []byte("old"), 0755); e != nil {
		t.Fatal(e)
	}
	raw := archiveFixture(t, []*tar.Header{{Name: "flats", Mode: 0755, Size: 3, Typeflag: tar.TypeReg}, {Name: "LICENSE", Mode: 0644, Size: 2, Typeflag: tar.TypeReg}})
	o := Options{Path: p, Version: "v1.2.3", GOOS: "linux", GOARCH: "amd64", ConfigDir: filepath.Join(dir, "config"), Home: filepath.Join(dir, "home"), RequiredContract: 2, Probe: func(context.Context, string) (Identity, error) {
		return Identity{Info: buildinfo.Info{Version: "v1.2.3", Release: true, OS: "linux", Arch: "amd64"}, ClientContract: 2}, nil
	}}
	o.Transport = fixtureTransport(raw, Digest(raw)+"  flats_linux_amd64.tar.gz\n")
	return o, raw
}
func fixtureTransport(raw []byte, sums string) transportFunc {
	return func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "github.com" || !strings.HasPrefix(r.URL.Path, "/gosuda/flats/releases/download/v1.2.3/") {
			return nil, fmt.Errorf("unexpected URL %s", r.URL)
		}
		b := raw
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			b = []byte(sums)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header), Request: r}, nil
	}
}
func assertOld(t *testing.T, o Options) {
	t.Helper()
	b, e := os.ReadFile(o.Path)
	if e != nil || string(b) != "old" {
		t.Fatalf("original not preserved: %q %v", b, e)
	}
	if _, e = os.Lstat(stageName(o.Path)); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("staging remained: %v", e)
	}
}
func TestUpdateSuccess(t *testing.T) {
	o, _ := setup(t)
	called := false
	o.Recheck = func() error { called = true; return nil }
	id, p, e := Update(t.Context(), o)
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(p)
	if e != nil || string(b) != "xxx" || id.ClientContract != 2 || !called {
		t.Fatalf("result %q %+v %v", b, id, e)
	}
	unlock, e := lock(p, o.ConfigDir)
	if e != nil {
		t.Fatal(e)
	}
	unlock()
}
func TestUpdateFailuresPreserveOriginal(t *testing.T) {
	for _, name := range []string{"checksum", "duplicate", "missing checksum", "candidate", "contract", "dirty", "platform", "probe", "recheck", "unavailable", "cancelled", "traversal", "symlink entry", "duplicate entry", "missing binary", "oversize", "permission", "service", "service unreadable", "locked", "invalid tag"} {
		t.Run(name, func(t *testing.T) {
			o, raw := setup(t)
			ctx := t.Context()
			want := ""
			switch name {
			case "checksum":
				o.Transport = fixtureTransport(raw, strings.Repeat("0", 64)+"  flats_linux_amd64.tar.gz\n")
				want = "checksum"
			case "duplicate":
				o.Transport = fixtureTransport(raw, strings.Repeat(Digest(raw)+"  flats_linux_amd64.tar.gz\n", 2))
				want = "checksum"
			case "missing checksum":
				o.Transport = fixtureTransport(raw, "")
				want = "checksum"
			case "candidate", "contract", "dirty", "platform":
				o.Probe = func(context.Context, string) (Identity, error) {
					id := Identity{Info: buildinfo.Info{Version: o.Version, Release: true, OS: o.GOOS, Arch: o.GOARCH}, ClientContract: 2}
					switch name {
					case "candidate":
						id.Version = "v0.1.0"
					case "contract":
						id.ClientContract = 0
					case "dirty":
						id.Dirty = true
					case "platform":
						id.Arch = "arm64"
					}
					return id, nil
				}
				want = "candidate_mismatch"
			case "probe":
				o.Probe = func(context.Context, string) (Identity, error) { return Identity{}, errors.New("bad candidate") }
				want = "candidate_mismatch"
			case "recheck":
				o.Recheck = func() error { return failure("server_changed", "changed") }
				want = "server_changed"
			case "unavailable":
				o.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
				})
				want = "assets_unavailable"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "traversal", "symlink entry", "duplicate entry", "missing binary", "oversize":
				headers := []*tar.Header{{Name: "flats", Mode: 0755, Typeflag: tar.TypeReg, Size: 3}}
				switch name {
				case "traversal":
					headers = append(headers, &tar.Header{Name: "../evil", Typeflag: tar.TypeReg})
				case "symlink entry":
					headers[0].Typeflag = tar.TypeSymlink
					headers[0].Size = 0
					headers[0].Linkname = "other"
				case "duplicate entry":
					headers = append(headers, headers[0])
				case "missing binary":
					headers[0].Name = "other"
				case "oversize":
					headers[0].Size = 0
				}
				raw = archiveFixture(t, headers)
				o.Transport = fixtureTransport(raw, Digest(raw)+"  flats_linux_amd64.tar.gz\n")
				want = "assets_unavailable"
			case "permission":
				if e := os.Chmod(o.Path, 0555); e != nil {
					t.Fatal(e)
				}
				want = "filesystem"
			case "service":
				p := filepath.Join(o.Home, ".config/systemd/user/flats.service")
				os.MkdirAll(filepath.Dir(p), 0755)
				os.WriteFile(p, []byte("[Service]\nExecStart="+o.Path+" serve\n"), 0644)
				want = "service_binary"
			case "service unreadable":
				p := filepath.Join(o.Home, ".config/systemd/user/flats.service")
				os.MkdirAll(p, 0755)
				want = "service_binary"
			case "locked":
				unlock, e := lock(o.Path, o.ConfigDir)
				if e != nil {
					t.Fatal(e)
				}
				defer unlock()
				want = "locked"
			case "invalid tag":
				o.Version = "../../evil"
				want = "invalid_target"
			}
			_, _, e := Update(ctx, o)
			if e == nil {
				t.Fatal("expected refusal")
			}
			if want != "" && Code(e) != want {
				t.Fatalf("want %s got %s: %v", want, Code(e), e)
			}
			assertOld(t, o)
		})
	}
}
func TestUpdateRefusesSymlinkAndManagedPaths(t *testing.T) {
	o, _ := setup(t)
	real := o.Path
	o.Path = filepath.Join(filepath.Dir(real), "alias")
	if e := os.Symlink(real, o.Path); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Update(t.Context(), o); Code(e) != "invalid_target" {
		t.Fatal(e)
	}
	o.Path = filepath.Join(filepath.Dir(real), "Cellar/flats/1/flats")
	os.MkdirAll(filepath.Dir(o.Path), 0755)
	os.WriteFile(o.Path, []byte("old"), 0755)
	if _, _, e := Update(t.Context(), o); Code(e) != "package_managed" {
		t.Fatal(e)
	}
}
func TestReleaseHTTPSDowngrade(t *testing.T) {
	c := releaseClient(transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://example.test/file"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	_, e := download(t.Context(), c, "https://github.com/test", 100)
	if e == nil || !strings.Contains(e.Error(), "downgrade") {
		t.Fatal(e)
	}
}
func TestDownloadSizeLimit(t *testing.T) {
	c := releaseClient(transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("1234")), Request: r}, nil
	}))
	if _, e := download(t.Context(), c, "https://github.com/test", 3); Code(e) != "assets_unavailable" {
		t.Fatal(e)
	}
}
func TestServiceParsers(t *testing.T) {
	p, e := plistExecutable([]byte(`<plist><dict><key>Label</key><string>dev.flats.serve</string><key>ProgramArguments</key><array><string>/path/a &amp; b/flats</string><string>serve</string></array></dict></plist>`))
	if e != nil || p != "/path/a & b/flats" {
		t.Fatalf("%q %v", p, e)
	}
	p, e = unitExecutable("[Service]\nExecStart=\"/path/a b/flats\" serve\n")
	if e != nil || p != "/path/a b/flats" {
		t.Fatalf("%q %v", p, e)
	}
}
func TestDirection(t *testing.T) {
	for _, tt := range []struct{ from, to, want string }{{"v1.0.0", "v1.1.0", "upgrade"}, {"v2.0.0", "v1.1.0", "downgrade"}, {"v1.0.0-rc.2", "v1.0.0-rc.10", "upgrade"}, {"v1.0.0-rc.2", "v1.0.0", "upgrade"}, {"v1.0.0", "v1.0.0", "same"}, {"dev", "v1.0.0", "unordered"}} {
		t.Run(tt.from+"_"+tt.to, func(t *testing.T) {
			if got := Direction(tt.from, tt.to); got != tt.want {
				t.Fatalf("%s", got)
			}
		})
	}
}

func TestServiceXDG(t *testing.T) {
	o, _ := setup(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	p := filepath.Join(xdg, "systemd/user/flats.service")
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte("[Service]\nExecStart="+o.Path+" serve\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Update(t.Context(), o); Code(e) != "service_binary" {
		t.Fatal(e)
	}
	assertOld(t, o)
}
func TestUpdateExecutesCandidateProbe(t *testing.T) {
	o, _ := setup(t)
	o.Probe = nil
	o.GOOS = runtime.GOOS
	o.GOARCH = runtime.GOARCH
	script := []byte("#!/bin/sh\n[ \"$1\" = '--json' ] && [ \"$2\" = version ] || exit 2\nprintf '%s\\n' '{\"version\":\"v1.2.3\",\"release\":true,\"dirty\":false,\"client_contract\":2,\"os\":\"" + runtime.GOOS + "\",\"arch\":\"" + runtime.GOARCH + "\"}'\n")
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if e := tw.WriteHeader(&tar.Header{Name: "flats", Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(script))}); e != nil {
		t.Fatal(e)
	}
	tw.Write(script)
	tw.Close()
	gz.Close()
	raw := b.Bytes()
	o.Transport = fixtureTransport(raw, fmt.Sprintf("%s  flats_%s_%s.tar.gz\n", Digest(raw), o.GOOS, o.GOARCH))
	id, p, e := Update(t.Context(), o)
	if e != nil {
		t.Fatal(e)
	}
	if id.ClientContract != 2 {
		t.Fatal(id)
	}
	fresh, e := probe(t.Context(), p)
	if e != nil || fresh.Version != "v1.2.3" {
		t.Fatalf("%+v %v", fresh, e)
	}
}
