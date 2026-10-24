package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

// Anonymous public editing of boards and documents
// (docs/spec/board-public-edit.md, docs-public-edit.md and their TECH pages,
// section 2): a Public board or document has a public access level, view or
// edit. Applying edit pins a safe point first; the running board or document
// worker learns the level and its epoch (flats.access_seq) from
// publicEditTarget.SetPublicAccess and, per request, from the
// X-Flats-Public-Access and X-Flats-Access-Epoch headers.

// AccessEpochHeader carries flats.access_seq on Public requests of a
// document, next to board.PublicAccessHeader. Clients cannot send it.
const AccessEpochHeader = "X-Flats-Access-Epoch"

// publicEditType reports whether flats of content type t can be edited by
// anyone: boards and documents.
func publicEditType(t string) bool {
	return t == contenttype.Board || t == contenttype.Docs
}

// editNoun names what anyone can edit in a flat of type t, for messages.
func editNoun(t string) string {
	if t == contenttype.Docs {
		return "these documents"
	}
	return "this board"
}

// ErrPublicEditBlocked refuses anyone-can-edit while the quotas it needs
// are missing (PublicEditBlockedError lists them).
var ErrPublicEditBlocked = errors.New("public editing blocked")

// errPinsFull marks a pin refused because the pin quota is full.
var errPinsFull = errors.New("pin quota full")

// Problems that block anyone-can-edit.
const (
	ProblemDiskQuotaOff           = "disk_quota_off"
	ProblemRevisionQuotaUnlimited = "revision_quota_unlimited"
	ProblemPinQuotaFull           = "pin_quota_full"
)

// PublicEditProblem is one reason a board or document cannot be edited by
// anyone.
type PublicEditProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PublicEditBlockedError refuses anyone-can-edit and says why.
type PublicEditBlockedError struct {
	Problems []PublicEditProblem
}

func (e *PublicEditBlockedError) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = p.Message
	}
	return "anyone-can-edit cannot take effect: " + strings.Join(msgs, "; ")
}

func (e *PublicEditBlockedError) Unwrap() []error { return []error{ErrPublicEditBlocked, ErrConflict} }

var problemText = map[string]string{
	ProblemDiskQuotaOff: "the host disk quota is off (0); set a disk quota in Settings, Limits",
	ProblemPinQuotaFull: "every pin is in use, so the safe point cannot be pinned; unpin a revision in History or raise the pin quota",
}

// problem describes code for a flat of type t.
func problem(t, code string) PublicEditProblem {
	msg := problemText[code]
	if code == ProblemRevisionQuotaUnlimited {
		msg = "the board's revision quota is unlimited (0); set a revision quota for it or for the host"
		if t == contenttype.Docs {
			msg = "the documents' revision quota is unlimited (0); set a revision quota for them or for the host"
		}
	}
	return PublicEditProblem{Code: code, Message: msg}
}

func blocked(t string, codes ...string) error {
	e := &PublicEditBlockedError{}
	for _, c := range codes {
		e.Problems = append(e.Problems, problem(t, c))
	}
	return e
}

// publicEditTarget is what public editing asks of a running board or
// document worker (TECH 2.2). PublicPresence reports ok false when the
// counts are not current; they then read as 0.
type publicEditTarget interface {
	SafePoint(ctx context.Context, prev *safePointPrev) (seq int64, reuse bool, err error)
	SetPublicAccess(ctx context.Context, level string, epoch int64) error
	PublicPresence() (guests, viewers int, ok bool)
}

// safePointPrev is the earlier safe point a new one may reuse: the files
// its revision stores and, for a document, its content source.
type safePointPrev struct {
	Source string
	Files  []store.RevisionFile
}

// publicEditTargetOf returns the public editing interface of slug's
// running board or document, and the deployment it belongs to, or nil.
func (s *Service) publicEditTargetOf(slugName string) (publicEditTarget, *deployed) {
	d := s.currentDeployment(slugName)
	if d == nil || d.inst == nil {
		return nil, nil
	}
	if in, ok := d.inst.(*board.Instance); ok {
		return boardTarget{s: s, in: in}, d
	}
	if contenttype.FromManifest(d.version.Manifest) == contenttype.Docs {
		return docsTarget{s: s, slug: slugName}, d
	}
	return nil, nil
}

// boardTarget is the public editing interface of a running board.
type boardTarget struct {
	s  *Service
	in *board.Instance
}

func (b boardTarget) SafePoint(ctx context.Context, prev *safePointPrev) (int64, bool, error) {
	var files []board.File
	if prev != nil {
		files = boardFiles(prev.Files)
	}
	seq, reuse, err := b.s.publicBoardOf(b.in).SafePoint(ctx, files)
	return seq, reuse, boardErr(err)
}

func (b boardTarget) SetPublicAccess(ctx context.Context, level string, epoch int64) error {
	setBoardAccessEpoch(b.in, epoch)
	return b.s.publicBoardOf(b.in).SetPublicAccess(ctx, level)
}

func (b boardTarget) PublicPresence() (int, int, bool) {
	g, v := b.s.publicBoardOf(b.in).PublicPresence()
	return g, v, true
}

// accessEpochSetter is the board's setter of the access epoch it sends to
// clients as access_epoch.
type accessEpochSetter interface{ SetAccessEpoch(epoch int64) }

// setBoardAccessEpoch tells a board instance flats.access_seq. It is
// called after the board opens and before every level change.
func setBoardAccessEpoch(in *board.Instance, epoch int64) {
	if e, ok := any(in).(accessEpochSetter); ok {
		e.SetAccessEpoch(epoch)
	}
}

// docsTarget is the public editing interface of a running document
// worker, through its host operations (TECH 3.6). The caller holds the flat
// lock.
type docsTarget struct {
	s    *Service
	slug string
}

func (d docsTarget) SafePoint(ctx context.Context, prev *safePointPrev) (int64, bool, error) {
	body := map[string]any{"prev": nil}
	if prev != nil {
		docs := make([]map[string]string, 0, len(prev.Files))
		for _, f := range prev.Files {
			docs = append(docs, map[string]string{"path": f.Path, "hash": f.Hash})
		}
		body["prev"] = map[string]any{"source": prev.Source, "documents": docs}
	}
	var out struct {
		Seq   int64 `json:"seq"`
		Reuse bool  `json:"reuse"`
	}
	err := d.s.docsHost(ctx, d.slug, http.MethodPost, "safe_point", body, &out)
	return out.Seq, out.Reuse, err
}

func (d docsTarget) SetPublicAccess(ctx context.Context, level string, epoch int64) error {
	return d.s.docsHost(ctx, d.slug, http.MethodPost, "access", map[string]any{"level": level, "epoch": epoch}, nil)
}

func (d docsTarget) PublicPresence() (int, int, bool) {
	return d.s.docsPresenceNow(d.slug)
}

// publicBoard is what public editing asks of a running board instance.
type publicBoard interface {
	SetPublicAccess(ctx context.Context, level string) error
	SafePoint(ctx context.Context, prev []board.File) (seq int64, reuse bool, err error)
	ContentEquals(ctx context.Context, files []board.File) (bool, error)
	PublicPresence() (guests, viewers int)
}

// publicBoardOf returns the public editing interface of a running board.
func (s *Service) publicBoardOf(in *board.Instance) publicBoard {
	if s.publicBoardHook != nil {
		return s.publicBoardHook(in)
	}
	return in
}

// PublicPresence counts the guests and viewers on a Public board or
// document.
type PublicPresence struct {
	Guests  int `json:"guests"`
	Viewers int `json:"viewers"`
}

// publicPresence reads the presence of a running board or document, or
// nil. It never wakes a document.
func (s *Service) publicPresence(slugName string) *PublicPresence {
	target, _ := s.publicEditTargetOf(slugName)
	if target == nil {
		return nil
	}
	g, v, ok := target.PublicPresence()
	if !ok {
		g, v = 0, 0
	}
	return &PublicPresence{Guests: g, Viewers: v}
}

// PublicEditStatus is what anyone-can-edit needs, for the console to check
// before it asks (GET /flats/{slug}/public-edit).
type PublicEditStatus struct {
	Problems      []PublicEditProblem `json:"problems"`
	DiskQuota     int64               `json:"disk_quota"`
	RevisionQuota int                 `json:"revision_quota"`
	PinQuota      int                 `json:"pin_quota"`
	Pins          int                 `json:"pins"`
	// SafePoint is the flat's last safe point while that revision exists;
	// SafePointPinned reports whether it is still pinned.
	SafePoint       *int `json:"safe_point"`
	SafePointPinned bool `json:"safe_point_pinned"`
	// Listed reports that a permitted provider lists the flat publicly.
	Listed bool `json:"listed"`
}

// PublicEditPreflight reports whether a board or document can become
// editable by anyone now, and what it would use.
func (s *Service) PublicEditPreflight(ctx context.Context, slugName string) (PublicEditStatus, error) {
	if _, _, err := s.publicEditState(ctx, slugName); err != nil {
		return PublicEditStatus{}, err
	}
	if err := s.catchUpRevisions(ctx, slugName); err != nil {
		return PublicEditStatus{}, err
	}
	out, _, err := s.publicEditStatus(ctx, slugName)
	return out, err
}

// publicEditState returns the content state and type of a board or a
// document, refusing other flats. A document without a content state yet
// (it gets one at its first revision import) has the default quotas.
func (s *Service) publicEditState(ctx context.Context, slugName string) (store.ContentState, string, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return store.ContentState{}, "", err
	}
	t, err := s.flatType(ctx, f)
	if err != nil {
		return store.ContentState{}, "", err
	}
	switch t {
	case contenttype.Board:
		st, err := s.boardState(ctx, slugName)
		return st, t, err
	case contenttype.Docs:
		st, err := s.st.ContentState(ctx, slugName)
		if errors.Is(err, store.ErrNotFound) {
			return store.ContentState{Type: contenttype.Docs}, t, nil
		}
		return st, t, err
	case "":
		return store.ContentState{}, t, fmt.Errorf("%w: %s has no content yet", ErrInvalid, slugName)
	}
	return store.ContentState{}, t, fmt.Errorf("%w: only a board or a document can be edited by anyone, and %s is a %s", ErrInvalid, slugName, typeNoun(t))
}

// publicEditStatus checks the quotas anyone-can-edit needs (TECH 2.3 step
// 1): a disk quota, a revision quota, and a pin for the safe point unless
// the pinned earlier safe point can be reused. A board compares it with the
// live board, edits not yet recorded included. A document's text lives in
// its worker, so a pinned earlier safe point counts as reusable here and the
// worker's safe_point operation compares; when it records a new one and no
// pin is free, the approval fails with pin_quota_full then.
func (s *Service) publicEditStatus(ctx context.Context, slugName string) (PublicEditStatus, store.ContentState, error) {
	st, t, err := s.publicEditState(ctx, slugName)
	if err != nil {
		return PublicEditStatus{}, st, err
	}
	revisions, pinQuota := s.quotas(st)
	pins, err := s.st.CountPins(ctx, slugName)
	if err != nil {
		return PublicEditStatus{}, st, err
	}
	out := PublicEditStatus{Problems: []PublicEditProblem{}, DiskQuota: max(s.diskQuota(), 0), RevisionQuota: revisions, PinQuota: pinQuota, Pins: pins}
	reusable := false
	if st.SafePointRev > 0 {
		r, err := s.st.Revision(ctx, slugName, st.SafePointRev)
		switch {
		case err == nil:
			out.SafePoint, out.SafePointPinned = &r.Number, r.Pinned
			if r.Pinned {
				if reusable, err = s.safePointReusable(ctx, slugName, t, r.Number); err != nil {
					return out, st, err
				}
			}
		case !errors.Is(err, store.ErrNotFound):
			return out, st, err
		}
	}
	if out.DiskQuota <= 0 {
		out.Problems = append(out.Problems, problem(t, ProblemDiskQuotaOff))
	}
	if revisions <= 0 {
		out.Problems = append(out.Problems, problem(t, ProblemRevisionQuotaUnlimited))
	}
	if !reusable && pinQuota > 0 && pins >= pinQuota {
		out.Problems = append(out.Problems, problem(t, ProblemPinQuotaFull))
	}
	out.Listed = s.publiclyListed(ctx, slugName)
	return out, st, nil
}

// safePointReusable reports whether pinned revision n may serve as the
// safe point again: it holds the running board's current files, or, for a
// document, the worker may still find it equal (see publicEditStatus).
func (s *Service) safePointReusable(ctx context.Context, slugName, t string, n int) (bool, error) {
	if t == contenttype.Docs {
		return true, nil
	}
	if in := s.liveBoard(slugName); in != nil {
		return s.liveContentEquals(ctx, slugName, in, n)
	}
	return false, nil
}

// liveContentEquals reports whether revision n holds the running board's
// current files.
func (s *Service) liveContentEquals(ctx context.Context, slugName string, in *board.Instance, n int) (bool, error) {
	files, err := s.st.RevisionFiles(ctx, slugName, n)
	if err != nil {
		return false, err
	}
	ok, err := s.publicBoardOf(in).ContentEquals(ctx, boardFiles(files))
	return ok, boardErr(err)
}

// publiclyListed reports whether a permitted provider lists the flat in a
// public directory: Portal, unless the flat's listing is hidden.
func (s *Service) publiclyListed(ctx context.Context, slugName string) bool {
	if ok, err := s.st.ProviderPermitted(ctx, slugName, store.ProviderPortal); err != nil || !ok {
		return false
	}
	mode := store.ListingDefault
	if m, err := s.st.PortalListing(ctx, slugName); err == nil {
		mode = m
	}
	return !s.listingHidden(mode)
}

// prepareSafePointLocked checks the quotas and pins the safe point of a
// board before anyone-can-edit commits (TECH 2.3 steps 1 to 3). It returns
// the pinned revision. The caller holds publicEditMu and the flat lock; the
// pin is kept even when the commit fails later.
func (s *Service) prepareSafePointLocked(ctx context.Context, slugName string) (int, error) {
	in := s.liveBoard(slugName)
	if in == nil {
		return 0, fmt.Errorf("%w: board %s is not running; get_logs has details", ErrUnavailable, slugName)
	}
	// Import first, so the preflight compares the safe point with the
	// board's current files.
	if _, err := s.importBoardLocked(ctx, slugName, in); err != nil {
		return 0, err
	}
	status, _, err := s.publicEditStatus(ctx, slugName)
	if err != nil {
		return 0, err
	}
	if len(status.Problems) > 0 {
		return 0, &PublicEditBlockedError{Problems: status.Problems}
	}
	prev, err := s.safePointPrev(ctx, slugName, status.SafePoint)
	if err != nil {
		return 0, err
	}
	seq, reuse, err := boardTarget{s: s, in: in}.SafePoint(ctx, prev)
	if err != nil {
		return 0, err
	}
	if reuse {
		if status.SafePoint == nil {
			return 0, fmt.Errorf("%w: the board reused a safe point it was not given", ErrUnavailable)
		}
		// Pin before the import: retention in the import could otherwise
		// delete an unpinned earlier safe point.
		n := *status.SafePoint
		if err := s.pinSafePointLocked(ctx, slugName, contenttype.Board, n); err != nil {
			return 0, err
		}
		if _, err := s.importBoardLocked(ctx, slugName, in); err != nil {
			s.Event(ctx, slugName, "warn", "revision", "could not record the board revision yet: "+err.Error(), nil)
		}
		return n, nil
	}
	imp, err := s.importBoardSafePointLocked(ctx, slugName, in, seq)
	if err != nil {
		return 0, err
	}
	return importedSafePoint(contenttype.Board, imp.safePoint, imp.pinsFull)
}

// prepareDocsSafePointLocked is prepareSafePointLocked for a document
// (TECH 2.3 steps 1 to 3): it wakes the worker, imports, has the worker
// record the safe point and imports it pinned. The returned context holds
// the worker's lease until release, so the level is applied to the same
// worker. The caller holds publicEditMu and the flat lock, and calls release
// even after an error.
func (s *Service) prepareDocsSafePointLocked(ctx context.Context, slugName string) (context.Context, func(), int, error) {
	noop := func() {}
	if err := s.checkStorageJournal(slugName); err != nil {
		return ctx, noop, 0, err
	}
	d := s.currentDeployment(slugName)
	if d == nil || contenttype.FromManifest(d.version.Manifest) != contenttype.Docs {
		return ctx, noop, 0, fmt.Errorf("%w: document %s is not running; get_logs has details", ErrUnavailable, slugName)
	}
	ctx, release, err := s.acquireDocsLocked(ctx, slugName, d)
	if err != nil {
		return ctx, release, 0, err
	}
	if _, err := s.importDocsLocked(ctx, slugName); err != nil {
		return ctx, release, 0, err
	}
	status, _, err := s.publicEditStatus(ctx, slugName)
	if err != nil {
		return ctx, release, 0, err
	}
	if len(status.Problems) > 0 {
		return ctx, release, 0, &PublicEditBlockedError{Problems: status.Problems}
	}
	prev, err := s.safePointPrev(ctx, slugName, status.SafePoint)
	if err != nil {
		return ctx, release, 0, err
	}
	seq, reuse, err := docsTarget{s: s, slug: slugName}.SafePoint(ctx, prev)
	if err != nil {
		return ctx, release, 0, err
	}
	if reuse {
		if status.SafePoint == nil {
			return ctx, release, 0, fmt.Errorf("%w: the document reused a safe point it was not given", ErrUnavailable)
		}
		n := *status.SafePoint
		if err := s.pinSafePointLocked(ctx, slugName, contenttype.Docs, n); err != nil {
			return ctx, release, 0, err
		}
		if _, err := s.importDocsLocked(ctx, slugName); err != nil {
			s.Event(ctx, slugName, "warn", "revision", "could not record the document revision yet: "+err.Error(), nil)
		}
		return ctx, release, n, nil
	}
	imp, err := s.importDocsPinLocked(ctx, slugName, seq)
	if err != nil {
		return ctx, release, 0, err
	}
	n, err := importedSafePoint(contenttype.Docs, imp.safePoint, imp.pinsFull)
	return ctx, release, n, err
}

// safePointPrev reads the earlier safe point revision n, or nil.
func (s *Service) safePointPrev(ctx context.Context, slugName string, n *int) (*safePointPrev, error) {
	if n == nil {
		return nil, nil
	}
	r, err := s.st.Revision(ctx, slugName, *n)
	if err != nil {
		return nil, err
	}
	files, err := s.st.RevisionFiles(ctx, slugName, *n)
	if err != nil {
		return nil, err
	}
	return &safePointPrev{Source: r.Source, Files: files}, nil
}

// pinSafePointLocked pins a reused safe point, refusing with
// pin_quota_full when no pin is free.
func (s *Service) pinSafePointLocked(ctx context.Context, slugName, t string, n int) error {
	if _, err := s.pinLocked(ctx, slugName, n, true); err != nil {
		if errors.Is(err, errPinsFull) {
			return blocked(t, ProblemPinQuotaFull)
		}
		return err
	}
	return nil
}

// importedSafePoint checks that an import pinned the new safe point.
func importedSafePoint(t string, n int, pinsFull bool) (int, error) {
	if pinsFull {
		return 0, blocked(t, ProblemPinQuotaFull)
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: the safe point was recorded but not imported; try again", ErrUnavailable)
	}
	return n, nil
}

// levelTimeout bounds telling a running board or document its public
// access level.
const levelTimeout = 10 * time.Second

// applyLevelLocked makes a running board or document apply the public
// access level and its epoch (flats.access_seq), independent of the
// caller's context. A sleeping document is left alone: it applies the
// stored level when it wakes. A board or document that does not take a level
// other than edit is restarted: that closes every guest connection, and it
// opens again with the stored level. Stopping always succeeds, so a level
// other than edit is in effect when it returns; the note says what else
// happened. The caller holds the flat lock, after the level committed.
func (s *Service) applyLevelLocked(ctx context.Context, slugName, level string, epoch int64) (note string) {
	target, d := s.publicEditTargetOf(slugName)
	if target == nil {
		return ""
	}
	what := "board"
	if _, ok := target.(docsTarget); ok {
		what = "document"
	}
	ctx = context.WithoutCancel(ctx)
	lctx, cancel := context.WithTimeout(ctx, levelTimeout)
	defer cancel()
	err := target.SetPublicAccess(lctx, level, epoch)
	if err == nil || errors.Is(err, errNotRunningDocs) {
		return ""
	}
	s.Event(ctx, slugName, "error", "visibility", fmt.Sprintf("the running %s did not apply public access %q: %v", what, accessName(level), err), nil)
	if level == store.AccessEdit {
		// It keeps refusing guests until it runs at edit.
		return "the running " + what + " did not take the new level yet, so guests cannot join until it restarts (get_logs has details)"
	}
	lf := s.state(slugName)
	old := lf.cur.Swap(nil)
	if old == nil || old != d {
		// It was replaced meanwhile; the new one opened with the stored
		// level.
		if old != nil {
			lf.cur.Store(old)
		}
		return ""
	}
	stopInstance(old.inst, "")
	if rerr := s.restart(lctx, slugName, old); rerr != nil {
		return "guest editing ended: the " + what + " was stopped, and it could not start again (" + rerr.Error() + ")"
	}
	s.Event(ctx, slugName, "warn", "visibility", "the "+what+" restarted to end guest editing", nil)
	return "the " + what + " restarted to end guest editing"
}

// repairLevelLocked applies the stored level to a running board or
// document again when that level is not edit, so repeating Stop guest
// editing finishes a change whose notification did not complete. The caller
// holds the flat lock.
func (s *Service) repairLevelLocked(ctx context.Context, f store.Flat) string {
	if f.PublicAccess == store.AccessEdit {
		return ""
	}
	return s.applyLevelLocked(ctx, f.Slug, f.PublicAccess, f.AccessSeq)
}

func accessName(level string) string {
	if level == "" {
		return "none"
	}
	return level
}

// visibilityLabel names a visibility and public access level for events
// and messages.
func visibilityLabel(vis store.Visibility, access string) string {
	if access == "" {
		return string(vis)
	}
	return string(vis) + " (" + access + ")"
}

// markSafePoint marks the safe point of a board or document among revs
// while anyone can edit it.
func (s *Service) markSafePoint(ctx context.Context, slugName string, st store.ContentState, revs []store.Revision) {
	if !publicEditType(st.Type) || st.SafePointRev == 0 {
		return
	}
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil || f.PublicAccess != store.AccessEdit {
		return
	}
	for i := range revs {
		revs[i].SafePoint = revs[i].Number == st.SafePointRev
	}
}

// editQuotaProblems names the boards and documents at anyone-can-edit that settings
// values would leave without a disk or revision quota (TECH 2.5). The
// caller holds publicEditMu.
func (s *Service) editQuotaProblems(ctx context.Context, values map[string]string) error {
	disk, diskSet := values[SetDiskQuotaBytes]
	revisions, revSet := values[SetRevisionQuota]
	diskOff := diskSet && settingInt(disk) <= 0
	revOff := revSet && settingInt(revisions) <= 0
	if !diskOff && !revOff {
		return nil
	}
	slugs, err := s.st.FlatsWithAccess(ctx, store.AccessEdit)
	if err != nil {
		return err
	}
	var noDisk, noRevisions []string
	for _, slugName := range slugs {
		if diskOff {
			noDisk = append(noDisk, slugName)
		}
		if revOff {
			st, err := s.st.ContentState(ctx, slugName)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err == nil && st.RevisionQuota == nil {
				noRevisions = append(noRevisions, slugName)
			}
		}
	}
	var msgs []string
	if len(noDisk) > 0 {
		msgs = append(msgs, "the disk quota cannot be off (0) while anyone can edit "+strings.Join(noDisk, ", "))
	}
	if len(noRevisions) > 0 {
		msgs = append(msgs, "the default revision quota cannot be unlimited (0) while anyone can edit "+strings.Join(noRevisions, ", ")+", which use the default")
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; stop guest editing first", ErrConflict, strings.Join(msgs, "; "))
}

func settingInt(v string) int64 {
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// downgradeEdit sets every board or document at anyone-can-edit whose
// quotas the current settings no longer allow, after an offline
// configuration edit, back to view only, and records why (TECH 2.5).
func (s *Service) downgradeEdit(ctx context.Context) error {
	slugs, err := s.st.FlatsWithAccess(ctx, store.AccessEdit)
	if err != nil {
		return err
	}
	for _, slugName := range slugs {
		reason := ""
		st, t, err := s.publicEditState(ctx, slugName)
		switch {
		case err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, store.ErrNotFound):
			return err
		case err != nil:
			reason = "it is not a board or a document"
		case s.diskQuota() <= 0:
			reason = "the host disk quota is off (0)"
		default:
			if revisions, _ := s.quotas(st); revisions <= 0 {
				reason = "its revision quota is unlimited (0)"
			}
		}
		if reason == "" {
			continue
		}
		stays := "the flat stays Public, view only"
		switch t {
		case contenttype.Board:
			stays = "the board stays Public, view only"
		case contenttype.Docs:
			stays = "the documents stay Public, view only"
		}
		ev := store.Event{Time: s.now(), Level: "warn", Kind: "visibility",
			Message: "guest editing stopped at startup because " + reason + "; " + stays}
		if err := s.st.SetPublicAccess(ctx, slugName, store.AccessEdit, store.AccessView, ev); err != nil {
			return fmt.Errorf("stop guest editing of %s: %w", slugName, err)
		}
		s.logf("flat %s: %s", slugName, ev.Message)
	}
	return nil
}

// withPublicAccess sets the trusted public access headers of a Public
// request: the level for a board, the level and its epoch for a document.
// withRouteHeaders has removed any the client sent.
func withPublicAccess(r *http.Request, d *deployed, access string, epoch int64) *http.Request {
	if access == "" {
		return r
	}
	if _, ok := d.inst.(*board.Instance); ok {
		r.Header.Set(board.PublicAccessHeader, access)
	} else if contenttype.FromManifest(d.version.Manifest) == contenttype.Docs {
		r.Header.Set(board.PublicAccessHeader, access)
		r.Header.Set(AccessEpochHeader, strconv.FormatInt(epoch, 10))
	}
	return r
}
