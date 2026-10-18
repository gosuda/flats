package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func createModelVersion(t *testing.T, s *Service, flat, body string) int {
	t.Helper()
	lifecycleSave(t, s, flat, body)
	r, err := s.CreateVersion(t.Context(), flat, 0, "", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), r.Approval.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return execution(t, a).Version
}

func TestVersionModelQuotaAndExplicitDeletion(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	one, two, zero := 1, 2, 0
	if n := createModelVersion(t, s, "quota", "one"); n != 1 {
		t.Fatal(n)
	}
	if err := s.SetVersionQuotaOverride(ctx, "quota", &one); err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, s, "quota", "two")
	if _, err := s.CreateVersion(ctx, "quota", 0, "", ViaAPI); !errors.Is(err, ErrVersionQuota) {
		t.Fatalf("full request: %v", err)
	}
	if err := s.SetVersionQuotaOverride(ctx, "quota", &two); err != nil {
		t.Fatal(err)
	}
	pending, err := s.CreateVersion(ctx, "quota", 0, "", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetVersionQuotaOverride(ctx, "quota", &one); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Decide(ctx, pending.Approval.ID, true); !errors.Is(err, ErrVersionQuota) || a.Status != "failed" {
		t.Fatalf("approval quota: %+v %v", a, err)
	}
	v, err := s.GetVersion(ctx, "quota", 1)
	if err != nil || v.Pruned {
		t.Fatalf("lower quota deleted existing version: %+v %v", v, err)
	}
	preview, err := s.OpenPreview(ctx, "quota", 1)
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.DeleteVersion(ctx, "quota", 1, ViaMCP)
	if err != nil || request.Approval == nil {
		t.Fatalf("deletion request: %+v %v", request, err)
	}
	if _, err := s.GetVersion(ctx, "quota", 1); err != nil {
		t.Fatal("request deleted before decision", err)
	}
	if _, err := s.Decide(ctx, request.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.GetPreview(ctx, preview.Host); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("preview remains: %v", err)
	}
	if _, err := os.Stat(s.versionDir("quota", 1)); !os.IsNotExist(err) {
		t.Fatalf("files remain: %v", err)
	}
	if _, err := s.GetVersion(ctx, "quota", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted row: %v", err)
	}
	if n := createModelVersion(t, s, "quota", "two"); n != 2 {
		t.Fatalf("number reused: %d", n)
	}
	if _, err := s.DeleteVersion(ctx, "quota", 2, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVersionQuotaOverride(ctx, "quota", &zero); err != nil {
		t.Fatal(err)
	}
	if n := createModelVersion(t, s, "quota", "three"); n != 3 {
		t.Fatal(n)
	}
	f, err := s.GetFlat(ctx, "quota")
	if err != nil || f.VersionCount != 1 || f.EffectiveVersionQuota != 0 || f.VersionQuota == nil || f.Publication != "unpublished" {
		t.Fatalf("view: %+v %v", f, err)
	}
}

func TestVersionModelDeletionStaleLiveAndSnapshots(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	createModelVersion(t, s, "live", "one")
	createModelVersion(t, s, "live", "two")
	deletion, err := s.DeleteVersion(ctx, "live", 1, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Deploy(ctx, "live", 1, ViaAPI)
	var deploy *PendingApproval
	if !errors.As(err, &deploy) {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, deploy.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, deletion.Approval.ID, true); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("stale deletion: %v", err)
	}
	if _, err := s.DeleteVersion(ctx, "live", 1, ViaConsole); !errors.Is(err, ErrConflict) {
		t.Fatalf("live deleted: %v", err)
	}
	snapshot := filepath.Join(s.snapshotDir("live"), "before-v2-123.sqlite")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("independent"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteVersion(ctx, "live", 2, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(snapshot); err != nil || string(b) != "independent" {
		t.Fatalf("version deletion removed snapshot: %q %v", b, err)
	}
}

func TestVersionModelPrunedCountAndDiskAdmission(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	createModelVersion(t, s, "pruned-count", "one")
	if err := s.st.MarkPruned(ctx, "pruned-count", 1); err != nil {
		t.Fatal(err)
	}
	one := 1
	if err := s.SetVersionQuotaOverride(ctx, "pruned-count", &one); err != nil {
		t.Fatal(err)
	}
	if n := createModelVersion(t, s, "pruned-count", "two"); n != 2 {
		t.Fatal(n)
	}
	lifecycleSave(t, s, "disk", "disk body")
	pending, err := s.CreateVersion(ctx, "disk", 0, "", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	used := dirSize(s.flatDir("disk"))
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: strconv.FormatInt(used, 10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, pending.Approval.ID, true); !errors.Is(err, ErrDiskQuota) {
		t.Fatalf("disk creation: %v", err)
	}
	if vs, err := s.st.ListVersions(ctx, "disk"); err != nil || len(vs) != 0 {
		t.Fatalf("failed admission created version: %v %v", vs, err)
	}
}

func TestVersionModelInterruptedDeletionRecovery(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			s, _ := newTestService(t)
			ctx := context.Background()
			createModelVersion(t, s, "recover", "one")
			original := s.versionDir("recover", 1)
			staged := filepath.Join(s.flatDir("recover"), versionDeletePrefix+"1")
			if err := os.Rename(original, staged); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := s.st.CommitVersionDeletion(ctx, "recover", 1, "", json.RawMessage(`{}`), s.now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.recoverVersionDeletions(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("staged path remains: %v", err)
			}
			_, err := os.Stat(original)
			if committed && !os.IsNotExist(err) {
				t.Fatal("deleted storage restored")
			}
			if !committed && err != nil {
				t.Fatalf("retained version not restored: %v", err)
			}
			n, err := s.st.NextVersionNumber(ctx, "recover")
			if err != nil || n != 2 {
				t.Fatalf("number history: %d %v", n, err)
			}
		})
	}
}

func TestVersionModelRestartRecoversApprovedDeletion(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	newService := func() *Service {
		svc, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	s := newService()
	defer func() { s.Close(); st.Close() }()
	createModelVersion(t, s, "restart", "one")
	r, err := s.DeleteVersion(ctx, "restart", 1, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimApprovalAuthorized(ctx, r.Approval.ID, "fixture operator", s.now()); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(s.flatDir("restart"), versionDeletePrefix+"1")
	if err := os.Rename(s.versionDir("restart", 1), staged); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(ApprovalExecution{Status: "applied", CleanupPending: true, Version: 1, DataImpact: "none", HealthData: "not_run", LiveData: "untouched"})
	if err := st.CommitVersionDeletion(ctx, "restart", 1, r.Approval.ID, receipt, s.now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	st.Close()
	st, err = store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s = newService()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("startup left staged deletion: %v", err)
	}
	approval, err := st.GetApproval(ctx, r.Approval.ID)
	if err != nil || approval.Status != "approved" || approval.Result != "deleted version 1" || execution(t, approval).CleanupPending {
		t.Fatalf("recovered approval: %+v %v", approval, err)
	}
	f, err := s.GetFlat(ctx, "restart")
	if err != nil || f.NextVersion != 2 || f.VersionCount != 0 {
		t.Fatalf("restarted count/number: %+v %v", f, err)
	}
	if n := createModelVersion(t, s, "restart", "two"); n != 2 {
		t.Fatalf("reused number after restart: %d", n)
	}
}

func TestVersionModelPreviewCloseFailureKeepsVersion(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	createModelVersion(t, s, "close-failure", "one")
	p, err := s.OpenPreview(ctx, "close-failure", 1)
	if err != nil {
		t.Fatal(err)
	}
	network.mu.Lock()
	network.stopExposure[p.Host] = errors.New("preview remains reachable")
	network.mu.Unlock()
	if _, err := s.DeleteVersion(ctx, "close-failure", 1, ViaConsole); err == nil {
		t.Fatal("deletion ignored preview close failure")
	}
	if _, err := s.GetVersion(ctx, "close-failure", 1); err != nil {
		t.Fatalf("deleted version: %v", err)
	}
	if _, err := os.Stat(s.versionDir("close-failure", 1)); err != nil {
		t.Fatalf("deleted files: %v", err)
	}
}

// A real older SQLite reader prevents FULL checkpoint completion while writers
// can still commit. This exercises the database durability boundary rather than
// substituting a filesystem-only recovery helper.
func holdVersionCheckpoint(t *testing.T, s *Service) func() {
	t.Helper()
	if err := s.st.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", filepath.Join(s.cfg.DataDir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := reader.BeginTx(t.Context(), nil)
	if err != nil {
		reader.Close()
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM versions").Scan(&n); err != nil {
		tx.Rollback()
		reader.Close()
		t.Fatal(err)
	}
	release := func() { tx.Rollback(); reader.Close() }
	t.Cleanup(release)
	return release
}

func TestVersionModelCheckpointFailureKeepsRecoverableFiles(t *testing.T) {
	t.Parallel()
	for _, approved := range []bool{false, true} {
		t.Run(strconv.FormatBool(approved), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "flats.db"))
			if err != nil {
				t.Fatal(err)
			}
			var svc *Service
			t.Cleanup(func() {
				if svc != nil {
					svc.Close()
				}
				st.Close()
			})
			open := func() {
				svc, err = New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
				if err != nil {
					t.Fatal(err)
				}
			}
			open()
			createModelVersion(t, svc, "busy", "one")
			var request ActionResult
			if approved {
				request, err = svc.DeleteVersion(ctx, "busy", 1, ViaMCP)
				if err != nil {
					t.Fatal(err)
				}
			}
			release := holdVersionCheckpoint(t, svc)
			var before store.Approval
			if approved {
				before, err = svc.Decide(ctx, request.Approval.ID, true)
				if err != nil || before.Status != "approved" || !execution(t, before).CleanupPending || !strings.Contains(before.Result, "Restart Flats") {
					t.Fatalf("approved pending result: %+v %v", before, err)
				}
			} else {
				result, err := svc.DeleteVersion(ctx, "busy", 1, ViaConsole)
				if err != nil || result.Status != "cleanup_pending" || !result.CleanupPending || !strings.Contains(result.Message, "Restart Flats") {
					t.Fatalf("direct pending result: %+v %v", result, err)
				}
			}
			staged := filepath.Join(svc.flatDir("busy"), versionDeletePrefix+"1")
			if b, err := os.ReadFile(filepath.Join(staged, "index.html")); err != nil || string(b) != "one" {
				t.Fatalf("checkpoint failure removed recoverable bytes: %q %v", b, err)
			}
			if _, err := svc.st.GetVersion(ctx, "busy", 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("metadata deletion not applied: %v", err)
			}
			if _, err := svc.CreateVersion(ctx, "busy", 0, "", ViaAPI); !errors.Is(err, ErrConflict) {
				t.Fatalf("pending cleanup did not fence writes: %v", err)
			}
			if !approved {
				if err := svc.recoverVersionDeletions(ctx); err == nil {
					t.Fatal("recovery removed files despite failed durability check")
				}
				if _, err := os.Stat(staged); err != nil {
					t.Fatal("recovery lost staging", err)
				}
			}
			release()
			if approved {
				// Hide the deletion from the newest-200 display list. Startup
				// recovery must still find this older cleanup receipt.
				for i := range 201 {
					if err := st.InsertApproval(ctx, store.Approval{ID: fmt.Sprintf("later-%d", i), Flat: "busy", Action: "deploy", Params: json.RawMessage(`{}`), Status: "approved", Via: "console", RequestedAt: before.RequestedAt.Add(time.Duration(i+1) * time.Second)}); err != nil {
						t.Fatal(err)
					}
				}
				listed, err := st.ListApprovals(ctx, "approved")
				if err != nil || len(listed) != 200 {
					t.Fatalf("display-list fixture: %d %v", len(listed), err)
				}
				for _, a := range listed {
					if a.ID == before.ID {
						t.Fatal("old cleanup receipt is still in display list")
					}
				}
			}
			svc.Close()
			svc = nil
			st.Close()
			st, err = store.Open(filepath.Join(dir, "flats.db"))
			if err != nil {
				t.Fatal(err)
			}
			open()
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("startup failed to finish cleanup: %v", err)
			}
			if approved {
				after, err := svc.GetApproval(ctx, request.Approval.ID)
				if err != nil || after.Status != "approved" || execution(t, after).CleanupPending || after.Result != "deleted version 1" {
					t.Fatalf("approved cleanup receipt did not settle: %+v %v", after, err)
				}
				if after.DecidedBy != before.DecidedBy || !after.DecidedAt.Equal(*before.DecidedAt) || !after.AuthorizedAt.Equal(*before.AuthorizedAt) {
					t.Fatal("cleanup rewrote operator provenance")
				}
			}
			if n := createModelVersion(t, svc, "busy", "two"); n != 2 {
				t.Fatalf("number after cleanup: %d", n)
			}
		})
	}
}

// Deletion/recreation reproduces the old recovery failure: the historical
// receipt still names the reused slug. Rename is a noninterference control,
// since renaming also updates approvals.flat and already passed the old code.
func TestVersionModelPendingReceiptReusedIdentityAndRenameControl(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"rename", "delete"} {
		name := "delete_reuse_regression"
		if change == "rename" {
			name = "rename_noninterference_control"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "flats.db"))
			if err != nil {
				t.Fatal(err)
			}
			var svc *Service
			t.Cleanup(func() {
				if svc != nil {
					svc.Close()
				}
				st.Close()
			})
			open := func() {
				svc, err = New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
				if err != nil {
					t.Fatal(err)
				}
			}
			open()
			createModelVersion(t, svc, "reused", "identical content")
			if err := os.RemoveAll(svc.versionDir("reused", 1)); err != nil {
				t.Fatal(err)
			}
			request, err := svc.DeleteVersion(ctx, "reused", 1, ViaMCP)
			if err != nil {
				t.Fatal(err)
			}
			release := holdVersionCheckpoint(t, svc)
			before, err := svc.Decide(ctx, request.Approval.ID, true)
			if err != nil || before.Status != "approved" || !execution(t, before).CleanupPending {
				t.Fatalf("missing-files checkpoint outcome: %+v %v", before, err)
			}
			release()
			if change == "rename" {
				if _, err := svc.RenameSlug(ctx, "reused", "renamed", ViaConsole); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := svc.Delete(ctx, "reused", ViaConsole, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			// Identical hashes cannot distinguish these two different flats.
			if n := createModelVersion(t, svc, "reused", "identical content"); n != 1 {
				t.Fatalf("replacement flat number: %d", n)
			}
			fresh, err := svc.GetVersion(ctx, "reused", 1)
			if err != nil {
				t.Fatal(err)
			}
			svc.Close()
			svc = nil
			st.Close()
			st, err = store.Open(filepath.Join(dir, "flats.db"))
			if err != nil {
				t.Fatal(err)
			}
			open()
			after, err := svc.GetApproval(ctx, before.ID)
			if err != nil || after.Status != "approved" || execution(t, after).CleanupPending || after.Result != "deleted version 1" {
				t.Fatalf("historical receipt not settled: %+v %v", after, err)
			}
			if after.DecidedBy != before.DecidedBy || !after.DecidedAt.Equal(*before.DecidedAt) || !after.AuthorizedAt.Equal(*before.AuthorizedAt) {
				t.Fatal("recovery changed operator provenance")
			}
			retained, err := svc.GetVersion(ctx, "reused", 1)
			if err != nil || retained.Hash != fresh.Hash {
				t.Fatalf("replacement metadata changed: %+v %v", retained, err)
			}
			data, err := os.ReadFile(filepath.Join(svc.versionDir("reused", 1), "index.html"))
			if err != nil || string(data) != "identical content" {
				t.Fatalf("replacement files changed: %q %v", data, err)
			}
			if change == "rename" {
				if _, err := svc.GetFlat(ctx, "renamed"); err != nil {
					t.Fatal("renamed original flat lost", err)
				}
			}
		})
	}
}

func TestVersionModelSuccessfulDeletionIsInMainDatabase(t *testing.T) {
	s, dir := newTestService(t)
	createModelVersion(t, s, "durable-version", "one")
	if result, err := s.DeleteVersion(t.Context(), "durable-version", 1, ViaConsole); err != nil || result.CleanupPending {
		t.Fatalf("deletion: %+v %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "main-only.db")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	copy, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if _, err := copy.GetVersion(t.Context(), "durable-version", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted version returns without WAL: %v", err)
	}
	if n, err := copy.NextVersionNumber(t.Context(), "durable-version"); err != nil || n != 2 {
		t.Fatalf("durable highwater: %d %v", n, err)
	}
}

func TestVersionModelMissingFilesCanBeExplicitlyDeleted(t *testing.T) {
	s, _ := newTestService(t)
	createModelVersion(t, s, "missing-files", "one")
	if err := os.RemoveAll(s.versionDir("missing-files", 1)); err != nil {
		t.Fatal(err)
	}
	request, err := s.DeleteVersion(t.Context(), "missing-files", 1, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), request.Approval.ID, true)
	if err != nil || a.Status != "approved" || execution(t, a).CleanupPending {
		t.Fatalf("missing-file deletion: %+v %v", a, err)
	}
	if n, err := s.st.VersionCount(t.Context(), "missing-files"); err != nil || n != 0 {
		t.Fatalf("stranded quota: %d %v", n, err)
	}
}

func TestVersionModelHostDefaultLoweringRefusesPendingCreation(t *testing.T) {
	s, _ := newTestService(t)
	createModelVersion(t, s, "host-quota", "one")
	lifecycleSave(t, s, "host-quota", "two")
	request, err := s.CreateVersion(t.Context(), "host-quota", 0, "", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(t.Context(), map[string]string{SetVersionQuota: "1"}); err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), request.Approval.ID, true)
	if !errors.Is(err, ErrVersionQuota) || a.Status != "failed" {
		t.Fatalf("lowered host default admitted version: %+v %v", a, err)
	}
	if _, err := s.GetVersion(t.Context(), "host-quota", 1); err != nil {
		t.Fatal("lowering deleted history", err)
	}
}

func TestVersionModelLegacyApprovalMissingFrozenLiveIsTruthful(t *testing.T) {
	s, _ := newTestService(t)
	createModelVersion(t, s, "legacy-pending", "one")
	_, err := s.Deploy(t.Context(), "legacy-pending", 1, ViaAPI)
	var deploy *PendingApproval
	if !errors.As(err, &deploy) {
		t.Fatal(err)
	}
	if _, err := s.Decide(t.Context(), deploy.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, s, "legacy-pending", "two")
	d, err := s.GetDraft(t.Context(), "legacy-pending")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.providerKey(t.Context(), "legacy-pending")
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]any{"revision": d.Revision, "hash": d.Hash, "base_version": 1, "visibility": "private", "providers": key}
	request, err := s.requestFrozen(t.Context(), "legacy-pending", "publish", old, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), request.Approval.ID, true)
	if !errors.Is(err, ErrStaleApproval) || !strings.Contains(a.Result, "does not record the frozen live version") || strings.Contains(a.Result, "changed after") || !strings.Contains(a.Result, "create_version") {
		t.Fatalf("misleading upgrade refusal: %+v %v", a, err)
	}
}

func TestVersionModelDefaultRollbackSkipsDeletedAndPruned(t *testing.T) {
	s, _ := newTestService(t)
	for _, body := range []string{"one", "two", "three", "four"} {
		lifecycleSave(t, s, "available-rollback", body)
		lifecycleApprove(t, s, lifecycleRequest(t, s, "available-rollback"))
	}
	if _, err := s.DeleteVersion(t.Context(), "available-rollback", 3, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.st.MarkPruned(t.Context(), "available-rollback", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rollback(t.Context(), "available-rollback", 3, ViaAPI); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("explicit deleted target: %v", err)
	}
	_, err := s.Rollback(t.Context(), "available-rollback", 0, ViaAPI)
	var request *PendingApproval
	if !errors.As(err, &request) {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), request.Approval.ID, true)
	if err != nil || execution(t, a).Version != 1 || liveBytes(t, s, "available-rollback") != "one" {
		t.Fatalf("default rollback did not skip unavailable versions: %+v %v", a, err)
	}
}

func TestVersionModelSnapshotFailurePreservesLiveDataAndVersion(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(strconv.FormatBool(rollback), func(t *testing.T) {
			s, _ := newTestService(t)
			ctx := t.Context()
			liveStarts := 0
			s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
				body, err := os.ReadFile(filepath.Join(spec.Dir, "server.js"))
				if err != nil {
					return nil, err
				}
				if err := os.MkdirAll(spec.DataDir, 0700); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(spec.DataDir, "startup"), body, 0600); err != nil {
					return nil, err
				}
				if spec.DataDir == s.dataDirOf("snapshot-failure") {
					liveStarts++
				}
				return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) })}, nil
			})
			create := func(body string) {
				t.Helper()
				old, err := s.Snapshots("snapshot-failure")
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.SaveVersion(ctx, "snapshot-failure", []bundle.File{
					{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)},
					{Path: "server.js", Data: []byte(body)},
				}, SaveMeta{}, ViaAPI)
				if err != nil {
					t.Fatal(err)
				}
				r, err := s.CreateVersion(ctx, "snapshot-failure", 0, "", ViaAPI)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Decide(ctx, r.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
				after, err := s.Snapshots("snapshot-failure")
				if err != nil || len(old) != len(after) {
					t.Fatalf("creation took a data snapshot: %v %v %v", old, after, err)
				}
			}
			deploy := func(n int) {
				t.Helper()
				_, err := s.Deploy(ctx, "snapshot-failure", n, ViaAPI)
				var p *PendingApproval
				if !errors.As(err, &p) {
					t.Fatal(err)
				}
				if _, err := s.Decide(ctx, p.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			create("one")
			if liveStarts != 0 {
				t.Fatal("creation started runtime")
			}
			deploy(1)
			create("two")
			deploy(2)
			target := 1
			if !rollback {
				create("three")
				target = 3
			}
			if err := os.WriteFile(filepath.Join(s.dataDirOf("snapshot-failure"), "preserve"), []byte("user data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(s.snapshotDir("snapshot-failure")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.snapshotDir("snapshot-failure"), []byte("filesystem fault"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if rollback {
				_, err = s.Rollback(ctx, "snapshot-failure", target, ViaAPI)
			} else {
				_, err = s.Deploy(ctx, "snapshot-failure", target, ViaAPI)
			}
			var pending *PendingApproval
			if !errors.As(err, &pending) {
				t.Fatal(err)
			}
			a, err := s.Decide(ctx, pending.Approval.ID, true)
			if !errors.Is(err, ErrSnapshotFailed) || ErrorCategory(err) != "snapshot_failed" || execution(t, a).FailureCode != "snapshot_failed" || strings.Contains(err.Error(), "runtime_start_failed") {
				t.Fatalf("snapshot failure misclassified: %+v %v", a, err)
			}
			if !strings.Contains(err.Error(), "snapshot failed before deployment") {
				t.Fatalf("misleading human error: %v", err)
			}
			if liveStarts != 2 || liveBytes(t, s, "snapshot-failure") != "two" {
				t.Fatal("snapshot failure started new live runtime")
			}
			f, err := s.GetFlat(ctx, "snapshot-failure")
			if err != nil || f.LiveVersion != 2 {
				t.Fatalf("live version changed: %+v %v", f, err)
			}
			if _, err := s.GetVersion(ctx, "snapshot-failure", target); err != nil {
				t.Fatal("failure removed target version", err)
			}
			for file, want := range map[string]string{"preserve": "user data", "startup": "two"} {
				b, err := os.ReadFile(filepath.Join(s.dataDirOf("snapshot-failure"), file))
				if err != nil || string(b) != want {
					t.Fatalf("live %s changed: %q %v", file, b, err)
				}
			}
			d := execution(t, a)
			if d.DataImpact != "none" || d.HealthData != "isolated_copy" || d.LiveData != "untouched" {
				t.Fatalf("incorrect data receipt: %+v", d)
			}
		})
	}
}

func TestVersionModelDefaultRollbackPreservesPreviousOnlyHistory(t *testing.T) {
	for _, state := range []string{"available", "deleted", "pruned"} {
		t.Run(state, func(t *testing.T) {
			s, _ := newTestService(t)
			ctx := t.Context()
			createModelVersion(t, s, "previous-only", "one")
			createModelVersion(t, s, "previous-only", "two")
			legacy, err := s.st.GetFlat(ctx, "previous-only")
			if err != nil {
				t.Fatal(err)
			}
			legacy.LiveVersion = 1
			if err := s.st.UpdateFlat(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			if err := s.st.SetLive(ctx, "previous-only", 2, 1, "deploy", "", s.now()); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "deleted":
				if _, err := s.DeleteVersion(ctx, "previous-only", 1, ViaConsole); err != nil {
					t.Fatal(err)
				}
			case "pruned":
				if err := s.st.MarkPruned(ctx, "previous-only", 1); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.Rollback(ctx, "previous-only", 0, ViaAPI)
			if state != "available" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("unavailable previous-only target chosen: %v", err)
				}
				return
			}
			var pending *PendingApproval
			if !errors.As(err, &pending) {
				t.Fatal(err)
			}
			var params rollbackParams
			if err := json.Unmarshal(pending.Approval.Params, &params); err != nil || params.Version != 1 {
				t.Fatalf("previous-only target lost: %+v %v", params, err)
			}
		})
	}
}
