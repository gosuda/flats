package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/store"
)

// The real board instance answers SetPublicAccess and SafePoint once the
// board server unit implements them. Until then these tests run the core
// side against fakePublicBoard, which records a safe point as an agent
// write on the real instance, so the outbox, import and lock paths are the
// real ones.

// memPublic is a PublicNet for tests inside the package.
type memPublic struct{ memNet }

func (n *memPublic) SetHidden(string, bool) error { return nil }

// fakePublicBoard stands in for the public editing interface of a running
// board.
type fakePublicBoard struct {
	s    *Service
	slug string

	mu     sync.Mutex
	in     *board.Instance
	levels []string
	// safePoints counts SafePoint calls; recorded the entries it wrote.
	safePoints, recorded int
	// after, when set, runs on the instance after a new safe point entry,
	// as a member group closing in the same apply loop step.
	after func(in *board.Instance)
	// live, when set, is the board's current file list, as edits in an
	// open group not yet recorded; otherwise the imported head is.
	live []board.File
	// setErr fails SetPublicAccess; onSet runs first, and setCtxErr keeps
	// the state of the context SetPublicAccess received.
	setErr    error
	onSet     func()
	setCtxErr error
}

func (f *fakePublicBoard) SetPublicAccess(ctx context.Context, level string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onSet != nil {
		f.onSet()
	}
	f.setCtxErr = ctx.Err()
	if f.setErr != nil {
		return f.setErr
	}
	f.levels = append(f.levels, level)
	return nil
}

// equalsCurrent reports whether files are the board's current files.
func (f *fakePublicBoard) equalsCurrent(ctx context.Context, files []board.File) (bool, error) {
	if f.live != nil {
		return slices.Equal(files, f.live), nil
	}
	head, err := f.s.st.Head(ctx, f.slug)
	if err != nil {
		return false, err
	}
	return slices.EqualFunc(files, head, func(a board.File, b store.HeadFile) bool { return a.Path == b.Path && a.Hash == b.Hash }), nil
}

func (f *fakePublicBoard) ContentEquals(ctx context.Context, files []board.File) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.equalsCurrent(ctx, files)
}

// SafePoint reuses prev when it equals the current files, as the board
// does; otherwise it records an entry.
func (f *fakePublicBoard) SafePoint(ctx context.Context, prev []board.File) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.safePoints++
	if prev != nil {
		if same, err := f.equalsCurrent(ctx, prev); err != nil || same {
			return 0, same, err
		}
	}
	f.recorded++
	res, err := f.in.UpdateBoard(ctx, board.UpdateRequest{OpID: fmt.Sprintf("safe-point-%d", f.recorded),
		Ops: boardOpsRaw(fmt.Sprintf(`[{"op":"add","el":{"type":"sticky","text":"safe %d"}}]`, f.recorded)), Label: "Agent"}, 0)
	if err != nil {
		return 0, false, err
	}
	if f.after != nil {
		f.after(f.in)
	}
	return res.OutboxSeq, false, nil
}

func (f *fakePublicBoard) PublicPresence() (int, int) { return 3, 7 }

func (f *fakePublicBoard) lastLevel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.levels) == 0 {
		return "<none>"
	}
	return f.levels[len(f.levels)-1]
}

func boardOpsRaw(raw string) []engine.AgentOp {
	var ops []engine.AgentOp
	_ = json.Unmarshal([]byte(raw), &ops)
	return ops
}

// newPublicBoard starts a service with a Public network, a board "plan"
// permitted on Portal and the fake public editing interface.
func newPublicBoard(t *testing.T) (*Service, *fakePublicBoard) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}},
		Public: &memPublic{memNet{hosts: map[string]http.Handler{}}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	fake := &fakePublicBoard{s: s, slug: "plan"}
	s.publicBoardHook = func(in *board.Instance) publicBoard {
		fake.mu.Lock()
		fake.in = in
		fake.mu.Unlock()
		return fake
	}
	newBoard(t, s, "plan")
	if err := s.SetProviderPermission(context.Background(), "plan", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	return s, fake
}

// requestAccess asks for a visibility and level; it must wait for approval.
func requestAccess(t *testing.T, s *Service, vis store.Visibility, access string, via Via) store.Approval {
	t.Helper()
	r, err := s.SetVisibilityAccess(context.Background(), "plan", vis, access, via, "")
	if err != nil || r.Status != "pending_approval" || r.Approval == nil {
		t.Fatalf("request %s/%s: %+v %v", vis, access, r, err)
	}
	return *r.Approval
}

// setAccess requests and approves a visibility and level.
func setAccess(t *testing.T, s *Service, vis store.Visibility, access string) {
	t.Helper()
	a := requestAccess(t, s, vis, access, ViaConsole)
	if got, err := s.Decide(context.Background(), a.ID, true); err != nil || got.Status != "approved" {
		t.Fatalf("approve %s/%s: %+v %v", vis, access, got, err)
	}
}

func flatAccess(t *testing.T, s *Service) (store.Visibility, string) {
	t.Helper()
	f, err := s.st.GetFlat(context.Background(), "plan")
	if err != nil {
		t.Fatal(err)
	}
	return f.Visibility, f.PublicAccess
}

func pinnedRevisions(t *testing.T, s *Service) []int {
	t.Helper()
	revs, err := s.st.ListRevisions(context.Background(), "plan", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, r := range revs {
		if r.Pinned {
			out = append(out, r.Number)
		}
	}
	return out
}

func safePointRev(t *testing.T, s *Service) int {
	t.Helper()
	st, err := s.st.ContentState(context.Background(), "plan")
	if err != nil {
		t.Fatal(err)
	}
	return st.SafePointRev
}

// Anyone-can-edit takes effect only through an approval: the request
// changes nothing, the approval pins a safe point, commits the level and
// tells the running board, all without a lock timeout.
func TestPublicEditOnlyThroughApproval(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	if _, err := s.SetVisibilityAccess(ctx, "plan", store.Private, store.AccessView, ViaMCP, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("private with a level: %v", err)
	}
	if _, err := s.SetVisibilityAccess(ctx, "plan", store.Public, "all", ViaMCP, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown level: %v", err)
	}
	for _, via := range []Via{ViaMCP, ViaConsole} {
		a := requestAccess(t, s, store.Public, store.AccessEdit, via)
		var p visibilityParams
		if err := json.Unmarshal(a.Params, &p); err != nil || p.Access != store.AccessEdit || p.FromAccess != "" || p.AccessSeq == nil {
			t.Fatalf("frozen params %s: %+v %v", a.Params, p, err)
		}
		if vis, access := flatAccess(t, s); vis != store.Private || access != "" {
			t.Fatalf("a request changed the flat: %s %q", vis, access)
		}
		if via == ViaMCP {
			if _, err := s.Decide(ctx, a.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if fake.safePoints != 0 || len(pinnedRevisions(t, s)) != 0 {
		t.Fatalf("a request pinned: %d calls, pins %v", fake.safePoints, pinnedRevisions(t, s))
	}
	// The console's own request is the one pending now; approving it is the
	// console's self-approval.
	pending, err := s.st.ListApprovals(ctx, "pending")
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v %v", pending, err)
	}
	done := make(chan error, 1)
	go func() {
		dctx, cancel := context.WithTimeout(ctx, boardTickTimeout)
		defer cancel()
		got, err := s.Decide(dctx, pending[0].ID, true)
		if err == nil && got.Status != "approved" {
			err = fmt.Errorf("status %s: %s", got.Status, got.Result)
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if vis, access := flatAccess(t, s); vis != store.Public || access != store.AccessEdit {
		t.Fatalf("after approval: %s %q", vis, access)
	}
	sp := safePointRev(t, s)
	if sp == 0 || !slices.Equal(pinnedRevisions(t, s), []int{sp}) {
		t.Fatalf("safe point %d, pins %v", sp, pinnedRevisions(t, s))
	}
	if fake.lastLevel() != store.AccessEdit {
		t.Fatalf("board level = %s", fake.lastLevel())
	}
	fv, err := s.GetFlat(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if fv.PublicAccess != store.AccessEdit || fv.PublicPresence == nil || *fv.PublicPresence != (PublicPresence{Guests: 3, Viewers: 7}) || fv.PublicNotice != PublicEditNotice {
		t.Fatalf("flat view = access %q presence %+v notice %q", fv.PublicAccess, fv.PublicPresence, fv.PublicNotice)
	}
	page, err := s.ListRevisions(ctx, "plan", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Revisions {
		if r.SafePoint != (r.Number == sp) {
			t.Fatalf("revision %d safe_point = %t (safe point %d)", r.Number, r.SafePoint, sp)
		}
	}
	// Stop guest editing: the level drops and the board hears it.
	setAccess(t, s, store.Public, store.AccessView)
	if _, access := flatAccess(t, s); access != store.AccessView || fake.lastLevel() != store.AccessView {
		t.Fatalf("after stop: %q board %s", access, fake.lastLevel())
	}
	setAccess(t, s, store.Private, "")
	if vis, access := flatAccess(t, s); vis != store.Private || access != "" || fake.lastLevel() != "" {
		t.Fatalf("after private: %s %q board %q", vis, access, fake.lastLevel())
	}
}

// Only a board can be edited by anyone.
func TestPublicEditRefusesOtherTypes(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	if _, err := s.CreateFlat(ctx, "site", "Site", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetVisibilityAccess(ctx, "site", store.Public, store.AccessEdit, ViaMCP, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("edit on a website: %v", err)
	}
}

// An edit approval requested at Public view stays stale after the operator
// went Private, Public edit and back to view: the pair is the same, but the
// level changed in between (spec rule 2).
func TestPublicEditApprovalRoundTripIsStale(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessView)
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
	setAccess(t, s, store.Private, "")
	setAccess(t, s, store.Public, store.AccessEdit)
	setAccess(t, s, store.Public, store.AccessView)
	calls := fake.safePoints
	got, err := s.Decide(ctx, a.ID, true)
	if !errors.Is(err, ErrStaleApproval) || got.Status != "failed" {
		t.Fatalf("round trip approval = %+v %v", got, err)
	}
	if _, access := flatAccess(t, s); access != store.AccessView || fake.safePoints != calls {
		t.Fatalf("a stale approval changed the board: %q, %d safe point calls", access, fake.safePoints-calls)
	}
	var exec ApprovalExecution
	if err := json.Unmarshal(got.ResultData, &exec); err != nil || exec.FailureCode != "stale_approval" {
		t.Fatalf("receipt %s %v", got.ResultData, err)
	}
}

// A visibility approval stored before access levels applies only as view,
// even when its parameters claim edit.
func TestPublicEditLegacyApprovalAppliesAsView(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	f, err := s.st.GetFlat(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := s.providerKey(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"visibility": "public", "from": "private", "providers": providers, "flat_id": f.ID, "access": "edit"})
	a := store.Approval{ID: "apr-legacyaccess1", Flat: "plan", Action: "set_visibility", Params: raw, Status: "pending",
		Via: string(ViaMCP), RequestedAt: s.now(), IdempotencyKey: "legacy-access"}
	if err := s.st.InsertApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Decide(ctx, a.ID, true); err != nil || got.Status != "approved" {
		t.Fatalf("legacy approval = %+v %v", got, err)
	}
	if vis, access := flatAccess(t, s); vis != store.Public || access != store.AccessView {
		t.Fatalf("legacy approval applied as %s %q", vis, access)
	}
	if fake.safePoints != 0 {
		t.Fatalf("a legacy approval asked for a safe point")
	}
}

// Missing quotas fail the approval with public_edit_blocked and change
// nothing a guest could use: no level, no safe point, no pin.
func TestPublicEditPreflightBlocks(t *testing.T) {
	ctx := context.Background()
	zero := 0
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *Service)
		code  string
	}{
		{"disk quota off", func(t *testing.T, s *Service) {
			if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); err != nil {
				t.Fatal(err)
			}
		}, ProblemDiskQuotaOff},
		{"revision quota unlimited", func(t *testing.T, s *Service) {
			if err := s.SetRevisionQuotas(ctx, "plan", &zero, nil); err != nil {
				t.Fatal(err)
			}
		}, ProblemRevisionQuotaUnlimited},
		{"pins full", func(t *testing.T, s *Service) {
			one := 1
			if err := s.SetRevisionQuotas(ctx, "plan", nil, &one); err != nil {
				t.Fatal(err)
			}
			if err := s.PinRevision(ctx, "plan", 1, true); err != nil {
				t.Fatal(err)
			}
		}, ProblemPinQuotaFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newPublicBoard(t)
			setAccess(t, s, store.Public, store.AccessView)
			tc.setup(t, s)
			status, err := s.PublicEditPreflight(ctx, "plan")
			if err != nil || len(status.Problems) != 1 || status.Problems[0].Code != tc.code {
				t.Fatalf("preflight = %+v %v", status, err)
			}
			pins := pinnedRevisions(t, s)
			revs := storedRevisions(t, s, "plan")
			a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
			got, err := s.Decide(ctx, a.ID, true)
			var blockedErr *PublicEditBlockedError
			if !errors.As(err, &blockedErr) || ErrorCategory(err) != "public_edit_blocked" || got.Status != "failed" {
				t.Fatalf("decide = %+v %v", got, err)
			}
			if len(blockedErr.Problems) != 1 || blockedErr.Problems[0].Code != tc.code {
				t.Fatalf("problems = %+v", blockedErr.Problems)
			}
			if _, access := flatAccess(t, s); access != store.AccessView {
				t.Fatalf("level = %q", access)
			}
			if fake.safePoints != 0 || !slices.Equal(pinnedRevisions(t, s), pins) || !slices.Equal(storedRevisions(t, s, "plan"), revs) {
				t.Fatalf("a blocked approval changed history: %d calls, pins %v revisions %v", fake.safePoints, pinnedRevisions(t, s), storedRevisions(t, s, "plan"))
			}
			if lvl := fake.lastLevel(); lvl == store.AccessEdit {
				t.Fatal("the board was told edit")
			}
		})
	}
}

// While anyone can edit a board, no quota change may leave it without a
// revision or disk quota (spec rule 6a).
func TestPublicEditQuotaTransitions(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	zero, fifty := 0, 50
	if err := s.SetRevisionQuotas(ctx, "plan", &zero, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("per-flat 0: %v", err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("disk quota 0: %v", err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "0"}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("default revision quota 0: %v", err)
	}
	// With its own revision quota the board allows a default of 0, but the
	// override cannot then be cleared.
	if err := s.SetRevisionQuotas(ctx, "plan", &fifty, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "0"}); err != nil {
		t.Fatalf("default 0 with an override: %v", err)
	}
	if err := s.SetRevisionQuotas(ctx, "plan", nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("clearing the override to a default of 0: %v", err)
	}
	if st, err := s.st.ContentState(ctx, "plan"); err != nil || st.RevisionQuota == nil || *st.RevisionQuota != 50 {
		t.Fatalf("override after refusals = %+v %v", st.RevisionQuota, err)
	}
	// After Stop guest editing the same changes are allowed.
	setAccess(t, s, store.Public, store.AccessView)
	if err := s.SetRevisionQuotas(ctx, "plan", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); err != nil {
		t.Fatal(err)
	}
}

// A host setting change to 0 racing an edit approval never leaves a board
// at edit without the quota.
func TestPublicEditRacesHostSettings(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessView)
	for i := range 8 {
		key := SetDiskQuotaBytes
		if i%2 == 1 {
			key = SetRevisionQuota
		}
		a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.Decide(ctx, a.ID, true) }()
		go func() { defer wg.Done(); _, _ = s.UpdateSettings(ctx, map[string]string{key: "0"}) }()
		wg.Wait()
		_, access := flatAccess(t, s)
		st, err := s.st.ContentState(ctx, "plan")
		if err != nil {
			t.Fatal(err)
		}
		revisions, _ := s.quotas(st)
		if access == store.AccessEdit && (s.diskQuota() <= 0 || revisions <= 0) {
			t.Fatalf("round %d: edit with disk quota %d and revision quota %d", i, s.diskQuota(), revisions)
		}
		if access == store.AccessEdit {
			setAccess(t, s, store.Public, store.AccessView)
		}
		if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "1000000000", SetRevisionQuota: "100"}); err != nil {
			t.Fatal(err)
		}
	}
}

// The safe point cannot be unpinned while anyone can edit, from any
// surface; afterwards it is an ordinary pin. Re-enabling an unchanged board
// pins the same safe point again and adds no revision.
func TestPublicEditSafePointProtection(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	sp := safePointRev(t, s)
	if err := s.PinRevision(ctx, "plan", sp, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("unpin while edit: %v", err)
	}
	backend := boardBackend{s: s, slug: "plan"}
	var be *board.Error
	if err := backend.Pin(ctx, sp, false); !errors.As(err, &be) || be.Category != board.CategoryConflict {
		t.Fatalf("board History unpin: %v", err)
	}
	setAccess(t, s, store.Public, store.AccessView)
	revs := storedRevisions(t, s, "plan")
	setAccess(t, s, store.Public, store.AccessEdit)
	if safePointRev(t, s) != sp || !slices.Equal(storedRevisions(t, s, "plan"), revs) || fake.recorded != 1 {
		t.Fatalf("re-enable: safe point %d (was %d), revisions %v (were %v), %d entries", safePointRev(t, s), sp, storedRevisions(t, s, "plan"), revs, fake.recorded)
	}
	setAccess(t, s, store.Public, store.AccessView)
	if err := s.PinRevision(ctx, "plan", sp, false); err != nil {
		t.Fatalf("unpin after stop: %v", err)
	}
	// Unpinned and every pin in use: re-enabling needs a pin and is blocked.
	one := 1
	if err := s.SetRevisionQuotas(ctx, "plan", nil, &one); err != nil {
		t.Fatal(err)
	}
	other := storedRevisions(t, s, "plan")[0]
	if other == sp {
		other = storedRevisions(t, s, "plan")[1]
	}
	if err := s.PinRevision(ctx, "plan", other, true); err != nil {
		t.Fatal(err)
	}
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
	if _, err := s.Decide(ctx, a.ID, true); !errors.Is(err, ErrPublicEditBlocked) {
		t.Fatalf("re-enable with full pins: %v", err)
	}
}

// The safe point is pinned inside the import that inserts it, so retention
// in that import keeps it even with a revision quota of 1 and a member
// entry recorded after it.
func TestPublicEditSafePointSurvivesRetention(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	one := 1
	if err := s.SetRevisionQuotas(ctx, "plan", &one, nil); err != nil {
		t.Fatal(err)
	}
	fake.after = func(in *board.Instance) {
		if _, err := in.UpdateBoard(ctx, board.UpdateRequest{OpID: "member-after-safe", Ops: boardOpsRaw(`[{"op":"add","el":{"type":"sticky","text":"later"}}]`), Label: "Ann"}, 0); err != nil {
			t.Error(err)
		}
	}
	setAccess(t, s, store.Public, store.AccessEdit)
	sp := safePointRev(t, s)
	revs := storedRevisions(t, s, "plan")
	if sp == 0 || !slices.Contains(revs, sp) || revs[0] == sp || len(revs) != 2 {
		t.Fatalf("safe point %d, revisions %v", sp, revs)
	}
	if !slices.Equal(pinnedRevisions(t, s), []int{sp}) {
		t.Fatalf("pins %v", pinnedRevisions(t, s))
	}
}

// Imported entries carry guest credits apart from authors, and a revision
// only guests edited is guest-only.
func TestBoardImportGuestCredits(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	unlock := s.lock("plan")
	defer unlock()
	in := s.liveBoard("plan")
	directBoardWrite(t, in, "guest-credit-1", `[{"op":"add","el":{"type":"sticky","text":"one"}}]`)
	directBoardWrite(t, in, "guest-credit-2", `[{"op":"add","el":{"type":"sticky","text":"two"}}]`)
	st, err := s.st.ContentState(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := in.Outbox(ctx, st.ImportSeq, boardImportBatch)
	if err != nil || len(entries) != 2 {
		t.Fatalf("outbox = %d %v", len(entries), err)
	}
	entries[0].Authors, entries[0].Guests, entries[0].GuestsMore = nil, []string{"Kim", "Lee", "Kim"}, 4
	entries[1].Guests = []string{"Park"}
	root := s.docsStoreDir("plan")
	for _, e := range entries {
		for _, h := range e.Snapshots {
			data, err := in.SnapshotData(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := putBlob(root, data); err != nil {
				t.Fatal(err)
			}
		}
	}
	res := boardImport{bySeq: map[int64]int{}}
	if err := s.commitBoardEntries(ctx, "plan", in.StoreID(), entries, 0, &res); err != nil {
		t.Fatal(err)
	}
	guestOnly, err := s.st.Revision(ctx, "plan", res.bySeq[entries[0].Seq])
	if err != nil {
		t.Fatal(err)
	}
	if !guestOnly.GuestOnly || len(guestOnly.Authors) != 0 || !slices.Equal(guestOnly.GuestAuthors, []string{"Kim", "Lee"}) || guestOnly.GuestsMore != 4 {
		t.Fatalf("guest-only revision = %+v", guestOnly)
	}
	mixed, err := s.st.Revision(ctx, "plan", res.bySeq[entries[1].Seq])
	if err != nil {
		t.Fatal(err)
	}
	if mixed.GuestOnly || len(mixed.Authors) == 0 || !slices.Equal(mixed.GuestAuthors, []string{"Park"}) {
		t.Fatalf("mixed revision = %+v", mixed)
	}
	info := boardRevisionInfo(guestOnly)
	if !slices.Equal(info.GuestAuthors, []string{"Kim", "Lee"}) || info.GuestsMore != 4 || len(info.Authors) != 0 {
		t.Fatalf("revision info = %+v", info)
	}
}

// An offline configuration edit that leaves an edit board without a quota
// returns it to view only at startup, with a visibility event.
func TestPublicEditStartupDowngrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}},
		Public: &memPublic{memNet{hosts: map[string]http.Handler{}}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	newBoard(t, s, "plan")
	if err := s.SetProviderPermission(ctx, "plan", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	a := requestAccess(t, s, store.Public, store.AccessView, ViaConsole)
	if _, err := s.Decide(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPublicAccess(ctx, "plan", store.AccessView, store.AccessEdit, store.Event{Level: "info", Kind: "visibility", Message: "test"}); err != nil {
		t.Fatal(err)
	}
	// As `flats config set` would leave it: an unlimited revision quota.
	zero := 0
	if err := st.ContentTx(ctx, "plan", func(tx *store.ContentTx) error { return tx.SetQuotas(ctx, &zero, nil) }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	st.Close()
	s2 := restartFrom(t, dir)
	f, err := s2.st.GetFlat(ctx, "plan")
	if err != nil || f.Visibility != store.Public || f.PublicAccess != store.AccessView {
		t.Fatalf("after restart: %+v %v", f, err)
	}
	events, err := s2.st.ListEvents(ctx, "plan", "visibility", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		found = found || strings.Contains(e.Message, "guest editing stopped at startup")
	}
	if !found {
		t.Fatalf("no startup event: %+v", events)
	}
}

// With the real board instance, a complete edit approval applies without a
// lock timeout and the safe point is a safe_point revision.
func TestPublicEditRealBoard(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	s.publicBoardHook = nil
	writeBoard(t, s, "plan", "real-board-1", `[{"op":"add","el":{"type":"sticky","text":"before guests"}}]`)
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaConsole)
	dctx, cancel := context.WithTimeout(ctx, boardTickTimeout)
	defer cancel()
	got, err := s.Decide(dctx, a.ID, true)
	if err != nil || got.Status != "approved" {
		t.Fatalf("decide = %+v %v", got, err)
	}
	sp := safePointRev(t, s)
	r, err := s.st.Revision(ctx, "plan", sp)
	if err != nil || r.Kind != board.KindSafePoint || !r.Pinned {
		t.Fatalf("safe point %d = %+v %v", sp, r, err)
	}
	// Re-enabling an unchanged board reuses it.
	setAccess(t, s, store.Public, store.AccessView)
	revs := storedRevisions(t, s, "plan")
	a = requestAccess(t, s, store.Public, store.AccessEdit, ViaConsole)
	if got, err := s.Decide(ctx, a.ID, true); err != nil || got.Status != "approved" {
		t.Fatalf("re-enable = %+v %v", got, err)
	}
	if safePointRev(t, s) != sp || !slices.Equal(storedRevisions(t, s, "plan"), revs) {
		t.Fatalf("re-enable added a revision: %v -> %v", revs, storedRevisions(t, s, "plan"))
	}
}

// A client cannot send the public access level; core sets it only for a
// running board on a Public route.
func TestPublicAccessHeaderIsTrusted(t *testing.T) {
	s, _ := newPublicBoard(t)
	r, _ := http.NewRequest("GET", "http://plan.test/", nil)
	r.Header.Set("x-flats-public-access", "edit")
	clean := trustedAccess(r, true)
	if v := clean.Header.Get(board.PublicAccessHeader); v != "" {
		t.Fatalf("forged level kept: %q", v)
	}
	in := s.liveBoard("plan")
	if got := withPublicAccess(trustedAccess(r, true), &deployed{inst: in}, store.AccessView).Header.Get(board.PublicAccessHeader); got != store.AccessView {
		t.Fatalf("board header = %q", got)
	}
	if got := withPublicAccess(trustedAccess(r, true), &deployed{}, store.AccessView).Header.Get(board.PublicAccessHeader); got != "" {
		t.Fatalf("non-board header = %q", got)
	}
}

// Stop guest editing reaches the running board even when the request is
// cancelled right after the level commits.
func TestPublicEditStopSurvivesCancellation(t *testing.T) {
	s, fake := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	a := requestAccess(t, s, store.Public, store.AccessView, ViaConsole)
	ctx, cancel := context.WithCancel(context.Background())
	fake.onSet = cancel
	_, _ = s.Decide(ctx, a.ID, true)
	if _, access := flatAccess(t, s); access != store.AccessView {
		t.Fatalf("level = %q", access)
	}
	if fake.lastLevel() != store.AccessView || fake.setCtxErr != nil {
		t.Fatalf("board level %q, its context %v", fake.lastLevel(), fake.setCtxErr)
	}
}

// A board that does not take Stop guest editing is restarted, which closes
// every guest connection; it opens again at the stored level.
func TestPublicEditStopRestartsUnresponsiveBoard(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	before := s.liveBoard("plan")
	fake.setErr = errors.New("apply loop stuck")
	a := requestAccess(t, s, store.Public, store.AccessView, ViaConsole)
	got, err := s.Decide(ctx, a.ID, true)
	if err != nil || got.Status != "approved" || !strings.Contains(got.Result, "restarted") {
		t.Fatalf("stop = %+v %v", got, err)
	}
	after := s.liveBoard("plan")
	if after == nil || after == before {
		t.Fatalf("board not restarted: before %p after %p", before, after)
	}
	if _, err := before.GetBoard(ctx, board.Query{Limit: 1}); err == nil {
		t.Fatal("the old instance still answers")
	}
	if _, err := after.GetBoard(ctx, board.Query{Limit: 1}); err != nil {
		t.Fatalf("the restarted board: %v", err)
	}
}

// Repeating Stop guest editing, as a request or as an approval, applies the
// stored level again to a board that missed it.
func TestPublicEditStopRetryRepairs(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	// The level committed, but the board never heard it.
	if err := s.st.SetPublicAccess(ctx, "plan", store.AccessEdit, store.AccessView, store.Event{Level: "info", Kind: "visibility", Message: "test"}); err != nil {
		t.Fatal(err)
	}
	if fake.lastLevel() != store.AccessEdit {
		t.Fatalf("board level = %q", fake.lastLevel())
	}
	r, err := s.SetVisibilityAccess(ctx, "plan", store.Public, store.AccessView, ViaConsole, "")
	if err != nil || r.Status != "done" || fake.lastLevel() != store.AccessView {
		t.Fatalf("retried stop = %+v %v, board level %q", r, err, fake.lastLevel())
	}
}

// Reuse of the safe point is decided from the live board: a member who
// restored its content in an open group re-enables editing with every pin in
// use, and no new safe point is recorded.
func TestPublicEditReuseFromLiveContent(t *testing.T) {
	ctx := context.Background()
	s, fake := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	sp := safePointRev(t, s)
	setAccess(t, s, store.Public, store.AccessView)
	writeBoard(t, s, "plan", "after-stop-1", `[{"op":"add","el":{"type":"sticky","text":"changed"}}]`)
	files, err := s.st.RevisionFiles(ctx, "plan", sp)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.live = boardFiles(files)
	fake.mu.Unlock()
	one := 1
	if err := s.SetRevisionQuotas(ctx, "plan", nil, &one); err != nil {
		t.Fatal(err)
	}
	status, err := s.PublicEditPreflight(ctx, "plan")
	if err != nil || len(status.Problems) != 0 || status.SafePoint == nil || *status.SafePoint != sp || !status.SafePointPinned {
		t.Fatalf("preflight = %+v %v", status, err)
	}
	revs := storedRevisions(t, s, "plan")
	setAccess(t, s, store.Public, store.AccessEdit)
	if safePointRev(t, s) != sp || fake.recorded != 1 || !slices.Equal(storedRevisions(t, s, "plan"), revs) {
		t.Fatalf("safe point %d (was %d), %d entries, revisions %v (were %v)", safePointRev(t, s), sp, fake.recorded, storedRevisions(t, s, "plan"), revs)
	}
}

// The preflight reports whether the earlier safe point is still pinned.
func TestPublicEditPreflightSafePointPinned(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	setAccess(t, s, store.Public, store.AccessEdit)
	sp := safePointRev(t, s)
	setAccess(t, s, store.Public, store.AccessView)
	if err := s.PinRevision(ctx, "plan", sp, false); err != nil {
		t.Fatal(err)
	}
	status, err := s.PublicEditPreflight(ctx, "plan")
	if err != nil || status.SafePoint == nil || *status.SafePoint != sp || status.SafePointPinned {
		t.Fatalf("preflight = %+v %v", status, err)
	}
	raw, _ := json.Marshal(status)
	if !strings.Contains(string(raw), `"safe_point_pinned":false`) {
		t.Fatalf("json = %s", raw)
	}
}
