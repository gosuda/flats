package core

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

type piCreationPath struct {
	name string
	typ  string
	run  func(*testing.T, *Service, string) error
}

// Transport names do not all have matching Service methods: create_flat
// dispatches by type, directory saves collect files before SaveVersion,
// save_page builds a page bundle, and save_design calls WriteDesign.
func piCreationPaths() []piCreationPath {
	return []piCreationPath{
		{"CreateFlat", "flat", func(t *testing.T, s *Service, slug string) error {
			_, err := s.CreateFlat(t.Context(), slug, "", ViaMCP)
			return err
		}},
		{"CreateFlatType/website", "flat", func(t *testing.T, s *Service, slug string) error {
			_, err := s.CreateFlat(t.Context(), slug, "Website", ViaMCP)
			return err
		}},
		{"CreateFlatType/page", "page", func(t *testing.T, s *Service, slug string) error {
			_, err := s.CreatePage(t.Context(), slug, "Page", ViaMCP)
			return err
		}},
		{"CreateFlatType/docs", "docs", func(t *testing.T, s *Service, slug string) error {
			_, err := s.CreateDocument(t.Context(), slug, "Document", ViaMCP)
			return err
		}},
		{"CreateFlatType/design", "design", func(t *testing.T, s *Service, slug string) error {
			_, _, err := s.CreateDesign(t.Context(), slug, "Design", nil, "", ViaMCP)
			return err
		}},
		{"CreateFlatType/board", "board", func(t *testing.T, s *Service, slug string) error {
			_, err := s.CreateBoard(t.Context(), slug, "Board", ViaMCP)
			return err
		}},
		{"SaveDraft", "flat", func(t *testing.T, s *Service, slug string) error {
			_, err := s.SaveDraft(t.Context(), slug, piCreationWebsite(), SaveMeta{}, 0, ViaMCP)
			return err
		}},
		{"SaveDraftFromDir", "flat", func(t *testing.T, s *Service, slug string) error {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "index.html"), piCreationWebsite()[0].Data, 0o600); err != nil {
				t.Fatal(err)
			}
			files, err := bundle.FromDir(dir, bundle.Limits{MaxBytes: s.UploadLimit()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.SaveVersion(t.Context(), slug, files, SaveMeta{}, ViaMCP)
			return err
		}},
		{"SavePage", "page", func(t *testing.T, s *Service, slug string) error {
			files := append(piCreationWebsite(), bundle.File{Path: bundle.ManifestName, Data: []byte(`{"type":"page"}`)})
			_, err := s.SaveVersion(t.Context(), slug, files, SaveMeta{}, ViaMCP)
			return err
		}},
		{"SaveDocument", "docs", func(t *testing.T, s *Service, slug string) error {
			_, err := s.SaveDocument(t.Context(), slug, "index.md", "# Creation content\n", "Document", SaveMeta{}, ViaMCP)
			return err
		}},
		{"SaveDesign", "design", func(t *testing.T, s *Service, slug string) error {
			_, err := s.WriteDesign(t.Context(), slug, DesignWrite{Files: []DesignFile{
				{Path: "canvas.json", Data: canvasJSON("a.html")},
				{Path: "a.html", Data: []byte(artboard)},
			}}, ViaMCP)
			return err
		}},
		{"UpdateBoard", "board", func(t *testing.T, s *Service, slug string) error {
			_, err := s.UpdateBoard(t.Context(), slug, BoardUpdate{OpID: "creation-update", Ops: boardOps(t, `[{"op":"add","el":{"type":"sticky","text":"Creation content"}}]`)}, ViaMCP)
			return err
		}},
		{"ImportBoard", "board", func(t *testing.T, s *Service, slug string) error {
			archive := piCreationBoardArchive(t)
			_, err := s.ImportBoard(t.Context(), slug, BoardImport{OpID: "creation-import", Mode: "add", Archive: bytes.NewReader(archive), Size: int64(len(archive))}, ViaMCP)
			return err
		}},
	}
}

func piCreationWebsite() []bundle.File {
	return []bundle.File{{Path: "index.html", Data: []byte(`<!doctype html><html><head><title>Creation</title></head><body>Creation content</body></html>`)}}
}

func piCreationBoardArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, file := range []struct{ name, content string }{
		{"board.json", `{"format":1,"title":"Creation content","grid":true}`},
		{"elements.json", `[{"id":"e_aaaaaaaaaa","type":"sticky","x":0,"y":0,"w":200,"h":100,"rot":0,"z":"V","text":"Creation content"}]`},
	} {
		w, err := zw.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func piCreationService(t *testing.T, on bool) (*Service, *lifecycleRouteNet) {
	t.Helper()
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	piCreationConfigure(t, s, on)
	return s, network
}

func piCreationConfigure(t *testing.T, s *Service, on bool) {
	t.Helper()
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	if _, err := s.SetPermittedNetworks([]string{"tailscale", "portal", "zrok"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(t.Context(), map[string]string{SetDefaultPrivate: "tailscale", SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	if on {
		if _, err := s.SetPublicInstance(t.Context(), true, "portal", "creation test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublicInstanceCreationPaths(t *testing.T) {
	for _, mode := range []struct {
		name string
		on   bool
	}{{"off", false}, {"on", true}} {
		for _, path := range piCreationPaths() {
			t.Run(mode.name+"/"+path.name, func(t *testing.T) {
				s, network := piCreationService(t, mode.on)
				if err := path.run(t, s, "created"); err != nil {
					t.Fatal(err)
				}
				f, err := s.GetFlat(t.Context(), "created")
				if err != nil {
					t.Fatal(err)
				}
				if f.Type != path.typ {
					t.Errorf("type = %q, want %q", f.Type, path.typ)
				}
				piCreationAssertPolicy(t, s, "created", mode.on)
				approvals, err := s.ListApprovals(t.Context(), "pending")
				if err != nil || len(approvals) != 0 {
					t.Errorf("creation requested approvals: %+v, %v", approvals, err)
				}
				if path.typ == "docs" || path.typ == "design" || path.typ == "board" {
					if !f.Running {
						t.Fatal("revision-model flat did not start at creation")
					}
					if mode.on && (!network.serves("created", ProviderPortal) || f.ConnectionState != "ready" || f.PublicURL == "") {
						t.Errorf("public creation has no ready route: %+v", f)
					}
				} else if mode.on && !strings.HasPrefix(path.name, "Save") && (f.LiveVersion != 0 || f.Publication != "unpublished" || f.PublicURL != "" || f.ConnectionState != "unavailable" || network.serves("created", ProviderPortal)) {
					t.Errorf("never-deployed flat is not Public + Unpublished: %+v", f)
				}
			})
		}
	}
}

func piCreationAssertPolicy(t *testing.T, s *Service, slug string, on bool) {
	t.Helper()
	f, err := s.st.GetFlat(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	visibility := store.Private
	wantProviders := []string{"local", "portal", "tailscale"}
	wantEvents := []string{
		"flat created via mcp",
		"provider tailscale permitted=true by default (network.default_private is tailscale)",
		"provider portal permitted=true by default (network.default_public is portal)",
	}
	if on {
		visibility = store.Public
		wantProviders = []string{"local", "portal"}
		wantEvents = []string{
			"flat created Public by public instance mode on portal via mcp",
			"provider portal permitted=true by default (network.default_public is portal)",
		}
	}
	access := ""
	if on {
		access = store.AccessView
	}
	if f.Visibility != visibility || f.PublicAccess != access {
		t.Errorf("visibility/access = %q/%q, want %q/%q", f.Visibility, f.PublicAccess, visibility, access)
	}
	providers, err := s.st.PermittedProviders(t.Context(), slug)
	if err != nil || !slices.Equal(providers, wantProviders) {
		t.Errorf("providers = %v, %v; want exactly %v", providers, err, wantProviders)
	}
	events, err := s.st.ListEvents(t.Context(), slug, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var gotEvents []string
	for _, e := range events {
		if e.Kind == "flat" || e.Kind == "provider" {
			gotEvents = append(gotEvents, e.Message)
			if e.Level != "info" || !e.Time.Equal(f.CreatedAt) {
				t.Errorf("creation event metadata = %+v, created_at = %s", e, f.CreatedAt)
			}
		}
	}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Errorf("creation events = %q, want exactly %q", gotEvents, wantEvents)
	}
}

func TestPublicInstanceCreationUnusableNetworkWritesNothing(t *testing.T) {
	for _, change := range []string{"clear_default", "disable_default_provider"} {
		for _, path := range piCreationPaths() {
			t.Run(change+"/"+path.name, func(t *testing.T) {
				s, network := piCreationService(t, true)
				if change == "clear_default" {
					if _, err := s.UpdateSettings(t.Context(), map[string]string{SetDefaultPublic: ""}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := s.SetPermittedNetworks([]string{"tailscale", "zrok"}, ""); err != nil {
					t.Fatal(err)
				}
				if err := path.run(t, s, "refused"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "no default public network is configured") {
					t.Errorf("unusable network: got %v, want conflict naming the missing default", err)
				}
				if !s.PublicInstance().Enabled {
					t.Error("network change silently turned public instance mode off")
				}
				if _, err := s.st.GetFlat(t.Context(), "refused"); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("flat was written: %v", err)
				}
				if _, err := os.Stat(s.flatDir("refused")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("flat storage was written: %v", err)
				}
				if events, err := s.st.ListEvents(t.Context(), "refused", "", 0, 100); err != nil || len(events) != 0 {
					t.Errorf("events were written: %+v, %v", events, err)
				}
				if providers, err := s.st.PermittedProviders(t.Context(), "refused"); err != nil || !slices.Equal(providers, []string{"local"}) {
					t.Errorf("provider grants were written: %v, %v", providers, err)
				}
				if versions, err := s.st.ListVersions(t.Context(), "refused"); err != nil || len(versions) != 0 {
					t.Errorf("versions were written: %+v, %v", versions, err)
				}
				if drafts, err := s.st.ListDraftRevisions(t.Context(), "refused"); err != nil || len(drafts) != 0 {
					t.Errorf("drafts were written: %+v, %v", drafts, err)
				}
				if revisions, err := s.st.ListRevisions(t.Context(), "refused", 0, 100); err != nil || len(revisions) != 0 {
					t.Errorf("revisions were written: %+v, %v", revisions, err)
				}
				if approvals, err := s.ListApprovals(t.Context(), "pending"); err != nil || len(approvals) != 0 {
					t.Errorf("approvals were written: %+v, %v", approvals, err)
				}
				network.mu.Lock()
				routes := len(network.routes)
				network.mu.Unlock()
				if routes != 0 {
					t.Errorf("registered %d routes for refused creation", routes)
				}
			})
		}
	}
}

func TestPublicInstanceCreationUnpublishedRestartRenameDeploy(t *testing.T) {
	for _, backend := range []string{"lifecycle", "legacy"} {
		for _, typ := range []string{"website", "page"} {
			t.Run(backend+"/"+typ, func(t *testing.T) {
				dir := t.TempDir()
				st, err := store.Open(filepath.Join(dir, "flats.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { st.Close() })
				network := newLifecycleRouteNet()
				files := piCreationWebsite()
				if typ == "page" {
					files = append(files, bundle.File{Path: bundle.ManifestName, Data: []byte(`{"type":"page"}`)})
				}
				cfg := func() Config {
					s := newLifecycleService(t, dir, st, network, nil)
					defer s.Close()
					piCreationConfigure(t, s, true)
					if backend == "legacy" {
						s.cfg.Lifecycle = nil
						s.cfg.Public = &memPublic{memNet{hosts: map[string]http.Handler{}}}
					}
					if _, err := s.CreateFlat(t.Context(), "before", "", ViaMCP); err != nil {
						t.Fatal(err)
					}
					if _, err := s.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
						t.Fatal(err)
					}
					if _, err := s.SaveDraft(t.Context(), "before", files, SaveMeta{}, 0, ViaMCP); err != nil {
						t.Fatal(err)
					}
					if _, err := s.SetPublicInstance(t.Context(), true, "portal", "test"); err != nil {
						t.Fatal(err)
					}
					piCreationAssertUnpublished(t, s, "before")
					return s.cfg
				}()
				s, err := New(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				if !s.PublicInstance().Enabled {
					t.Fatal("restart lost public instance mode")
				}
				piCreationAssertUnpublished(t, s, "before")
				if _, err := s.RenameSlug(t.Context(), "before", "after", ViaMCP); err != nil {
					t.Fatal(err)
				}
				piCreationAssertUnpublished(t, s, "after")
				if marked, err := s.st.GetFlat(t.Context(), "after"); err != nil || !marked.CreatedPublicInstance {
					t.Fatalf("restart/rename lost mark: %+v %v", marked, err)
				}
				if backend == "lifecycle" && (network.serves("before", ProviderPortal) || network.serves("after", ProviderPortal)) {
					t.Fatal("rename opened a public route before first deployment")
				}
				if backend == "legacy" {
					pub := cfg.Public.(*memPublic)
					pub.mu.Lock()
					n := len(pub.hosts)
					pub.mu.Unlock()
					if n != 0 {
						t.Fatalf("rename opened %d public routes before first deployment", n)
					}
				}
				publish, err := s.Publish(t.Context(), "after", 0, "", ViaMCP)
				if err != nil || publish.Status != "pending_approval" {
					t.Fatalf("first version bypassed approval: %+v, %v", publish, err)
				}
				piCreationAssertUnpublished(t, s, "after")
				if _, err := s.Decide(t.Context(), publish.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
				piCreationAssertUnpublished(t, s, "after")
				_, err = s.Deploy(t.Context(), "after", 1, ViaMCP)
				var pending *PendingApproval
				if !errors.As(err, &pending) {
					t.Fatalf("first deployment bypassed approval: %v", err)
				}
				piCreationAssertUnpublished(t, s, "after")
				if _, err := s.Decide(t.Context(), pending.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
				f, err := s.GetFlat(t.Context(), "after")
				if err != nil || f.Visibility != store.Public || f.Publication != "published" || f.LiveVersion != 1 || !f.Running || f.PublicURL == "" {
					t.Fatalf("first approved deployment = %+v, %v", f, err)
				}
				var h http.Handler
				if backend == "lifecycle" {
					network.mu.Lock()
					h = network.routes[lifecycleRouteKey{host: "after", provider: ProviderPortal}]
					network.mu.Unlock()
				} else {
					pub := cfg.Public.(*memPublic)
					pub.mu.Lock()
					h = pub.hosts["after"]
					pub.mu.Unlock()
				}
				if h == nil {
					t.Fatal("first deployment did not register its public handler")
				}
				r := httptest.NewRecorder()
				h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
				if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Creation content") {
					t.Errorf("first public response = %d %s", r.Code, r.Body.String())
				}
			})
		}
	}
}

func piCreationAssertUnpublished(t *testing.T, s *Service, slug string) {
	t.Helper()
	f, err := s.GetFlat(t.Context(), slug)
	if err != nil || f.Visibility != store.Public || f.LiveVersion != 0 || f.Publication != "unpublished" || f.PublicURL != "" || f.ConnectionState != "unavailable" || f.Running {
		t.Fatalf("want Public + Unpublished, got %+v, %v", f, err)
	}
	stored, err := s.st.GetFlat(t.Context(), slug)
	if err != nil || stored.PublicAccess != store.AccessView {
		t.Fatalf("stored public access = %q, %v; want view", stored.PublicAccess, err)
	}
	events, err := s.st.ListEvents(t.Context(), slug, "exposure", 0, 100)
	if err != nil || slices.ContainsFunc(events, func(e store.Event) bool { return e.Level == "error" }) {
		t.Fatalf("unpublished flat attempted exposure: %+v, %v", events, err)
	}
}

func TestPublicInstanceCreationExposureFailureRegrant(t *testing.T) {
	for _, path := range piCreationPaths() {
		if path.typ != "docs" && path.typ != "design" && path.typ != "board" {
			continue
		}
		t.Run(path.name, func(t *testing.T) {
			s, network := piCreationService(t, true)
			network.failOpen = map[ProviderID]error{ProviderPortal: fmt.Errorf("creation provider offline: %w", ErrProviderUnavailable)}
			if err := path.run(t, s, "recover"); err != nil {
				t.Fatalf("exposure failure undid creation: %v", err)
			}
			piCreationAssertPolicy(t, s, "recover", true)
			f, err := s.GetFlat(t.Context(), "recover")
			if err != nil || !f.Running || f.Visibility != store.Public || f.ConnectionState != "error" || f.PublicURL != "" {
				t.Fatalf("failed exposure result = %+v, %v", f, err)
			}
			createdID := f.ID
			if len(f.Endpoints) != 1 || f.Endpoints[0].Provider != ProviderPortal || f.Endpoints[0].State != "error" || !strings.Contains(f.Endpoints[0].Detail, "creation provider offline") {
				t.Errorf("provider error not reported: %+v", f.Endpoints)
			}
			events, err := s.st.ListEvents(t.Context(), "recover", "exposure", 0, 100)
			if err != nil || !slices.ContainsFunc(events, func(e store.Event) bool {
				return e.Level == "error" && strings.Contains(e.Message, "creation provider offline")
			}) {
				t.Errorf("missing exposure error event: %+v, %v", events, err)
			}
			switch path.typ {
			case "docs":
				stored, err := s.st.GetFlat(t.Context(), "recover")
				if err != nil {
					t.Fatal(err)
				}
				if b, err := os.ReadFile(filepath.Join(s.versionDir("recover", stored.LiveVersion), "index.md")); err != nil || len(b) == 0 {
					t.Fatalf("document content lost: %q, %v", b, err)
				}
			case "design":
				file := "main.html"
				if path.name == "SaveDesign" {
					file = "a.html"
				}
				b, _, err := s.DesignFileContent(t.Context(), "recover", file)
				if err != nil || len(b) == 0 || path.name == "SaveDesign" && string(b) != artboard {
					t.Fatalf("design content lost: %q, %v", b, err)
				}
			case "board":
				page, err := s.GetBoard(t.Context(), "recover", 0, board.Query{})
				want := 1
				if path.name == "CreateFlatType/board" {
					want = 0
				}
				if err != nil || len(page.Elements) != want || want == 1 && !bytes.Contains(page.Elements[0], []byte("Creation content")) {
					t.Fatalf("board content lost: %+v, %v", page, err)
				}
			}
			network.mu.Lock()
			delete(network.failOpen, ProviderPortal)
			network.mu.Unlock()
			if err := s.SetProviderPermission(t.Context(), "recover", "portal", true, ViaConsole); err != nil {
				t.Fatalf("idempotent regrant did not recover: %v", err)
			}
			f, err = s.GetFlat(t.Context(), "recover")
			if err != nil || f.ID != createdID || f.Visibility != store.Public || f.ConnectionState != "ready" || f.PublicURL == "" || !network.serves("recover", ProviderPortal) {
				t.Errorf("recovered public route = %+v, %v", f, err)
			}
			network.mu.Lock()
			h := network.routes[lifecycleRouteKey{host: "recover", provider: ProviderPortal}]
			network.mu.Unlock()
			if h == nil {
				t.Fatal("regrant left no public handler")
			}
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
			if r.Code != http.StatusOK {
				t.Errorf("recovered public response = %d %s", r.Code, r.Body.String())
			}
		})
	}
}
