package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
)

func TestDocsIdleCommitFailurePreservesOldAuthority(t *testing.T) {
	s, dir := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "notes")
	old := s.currentDeployment("notes")
	before, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := s.st.ListRevisions(t.Context(), "notes", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := s.Deployments(t.Context(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	s.runtimeMu.Lock()
	generation := s.runtimeOwners[s.dataDirOf("notes")]
	s.runtimeMu.Unlock()
	// Abort exactly the live-pointer update inside CommitPublished's transaction,
	// after candidate startup, while leaving ordinary draft writes available.
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER refuse_idle_commit BEFORE UPDATE OF live_version ON flats WHEN NEW.slug='notes' AND NEW.live_version<>OLD.live_version BEGIN SELECT RAISE(FAIL,'injected live commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	var candidate *idleTestInstance
	var candidateVersion int
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		if !spec.ManagedDocs {
			return lifecycleInstance{h}, nil
		}
		candidateVersion = spec.Version
		candidate = &idleTestInstance{Handler: h, wake: func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
			return nil, errors.New("readiness must not run before commit")
		}}
		return candidate, nil
	})
	_, err = s.SaveVersion(t.Context(), "notes", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# refused replacement")}}, SaveMeta{}, ViaAPI)
	var de *DeployError
	if !errors.As(err, &de) || de.Committed {
		t.Fatalf("expected precommit failure: %v", err)
	}
	if candidate == nil || candidate.wakes != 0 || candidate.stopped != 1 {
		t.Fatalf("candidate lifecycle: %+v", candidate)
	}
	after, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	if after.LiveVersion != before.LiveVersion || s.currentDeployment("notes") != old {
		t.Fatal("failed commit replaced old authority")
	}
	s.runtimeMu.Lock()
	gotGeneration := s.runtimeOwners[s.dataDirOf("notes")]
	s.runtimeMu.Unlock()
	if gotGeneration != generation {
		t.Fatalf("old runtime authority not restored: %d -> %d", generation, gotGeneration)
	}
	v, err := s.st.GetVersion(t.Context(), "notes", after.LiveVersion)
	if err != nil || v.Hash != old.version.Hash {
		t.Fatalf("old hash lost: %+v %v", v, err)
	}
	afterRevisions, err := s.st.ListRevisions(t.Context(), "notes", 0, 100)
	if err != nil || len(afterRevisions) != len(revisions) {
		t.Fatalf("failed activation recorded revision: %v %v", afterRevisions, err)
	}
	afterDeployments, err := s.Deployments(t.Context(), "notes")
	if err != nil || len(afterDeployments) != len(deployments) {
		t.Fatalf("failed transaction recorded deployment: %v %v", afterDeployments, err)
	}
	if _, err := os.Stat(s.versionDir("notes", candidateVersion)); !os.IsNotExist(err) {
		t.Fatalf("failed candidate files retained: %v", err)
	}
}
