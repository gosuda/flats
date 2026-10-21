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

// Anonymous public editing of boards (docs/spec/board-public-edit.md,
// board-public-edit-TECH.md 2): a Public board has a public access level,
// view or edit. Applying edit pins a safe point first; the running board
// learns the level from Instance.SetPublicAccess and, per request, from the
// X-Flats-Public-Access header.

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

// PublicEditProblem is one reason a board cannot be edited by anyone.
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
	ProblemDiskQuotaOff:           "the host disk quota is off (0); set a disk quota in Settings, Limits",
	ProblemRevisionQuotaUnlimited: "the board's revision quota is unlimited (0); set a revision quota for it or for the host",
	ProblemPinQuotaFull:           "every pin is in use, so the safe point cannot be pinned; unpin a revision in History or raise the pin quota",
}

func problem(code string) PublicEditProblem {
	return PublicEditProblem{Code: code, Message: problemText[code]}
}

func blocked(codes ...string) error {
	e := &PublicEditBlockedError{}
	for _, c := range codes {
		e.Problems = append(e.Problems, problem(c))
	}
	return e
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

// PublicPresence counts the guests and viewers on a running Public board.
type PublicPresence struct {
	Guests  int `json:"guests"`
	Viewers int `json:"viewers"`
}

// publicPresence reads the presence of a running board, or nil.
func (s *Service) publicPresence(slugName string) *PublicPresence {
	in := s.liveBoard(slugName)
	if in == nil {
		return nil
	}
	g, v := s.publicBoardOf(in).PublicPresence()
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
	// SafePoint is the board's last safe point while that revision exists;
	// SafePointPinned reports whether it is still pinned.
	SafePoint       *int `json:"safe_point"`
	SafePointPinned bool `json:"safe_point_pinned"`
	// Listed reports that a permitted provider lists the flat publicly.
	Listed bool `json:"listed"`
}

// PublicEditPreflight reports whether a board can become editable by anyone
// now, and what it would use.
func (s *Service) PublicEditPreflight(ctx context.Context, slugName string) (PublicEditStatus, error) {
	if err := s.catchUpBoard(ctx, slugName); err != nil {
		return PublicEditStatus{}, err
	}
	out, _, err := s.publicEditStatus(ctx, slugName)
	return out, err
}

// publicEditStatus checks the quotas anyone-can-edit needs (TECH 2.3 step
// 1): a disk quota, a revision quota, and a pin for the safe point unless
// the pinned earlier safe point still matches the live board, edits not
// yet recorded included.
func (s *Service) publicEditStatus(ctx context.Context, slugName string) (PublicEditStatus, store.ContentState, error) {
	st, err := s.boardState(ctx, slugName)
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
			if in := s.liveBoard(slugName); in != nil && r.Pinned {
				if reusable, err = s.liveContentEquals(ctx, slugName, in, r.Number); err != nil {
					return out, st, err
				}
			}
		case !errors.Is(err, store.ErrNotFound):
			return out, st, err
		}
	}
	if out.DiskQuota <= 0 {
		out.Problems = append(out.Problems, problem(ProblemDiskQuotaOff))
	}
	if revisions <= 0 {
		out.Problems = append(out.Problems, problem(ProblemRevisionQuotaUnlimited))
	}
	if !reusable && pinQuota > 0 && pins >= pinQuota {
		out.Problems = append(out.Problems, problem(ProblemPinQuotaFull))
	}
	out.Listed = s.publiclyListed(ctx, slugName)
	return out, st, nil
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

// prepareSafePointLocked checks the quotas and pins the safe point before
// anyone-can-edit commits (TECH 2.3 steps 1 to 3). It returns the pinned
// revision. The caller holds publicEditMu and the flat lock; the pin is kept
// even when the commit fails later.
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
	var prev []board.File
	if status.SafePoint != nil {
		files, err := s.st.RevisionFiles(ctx, slugName, *status.SafePoint)
		if err != nil {
			return 0, err
		}
		prev = boardFiles(files)
	}
	seq, reuse, err := s.publicBoardOf(in).SafePoint(ctx, prev)
	if err != nil {
		return 0, boardErr(err)
	}
	if reuse {
		if status.SafePoint == nil {
			return 0, fmt.Errorf("%w: the board reused a safe point it was not given", ErrUnavailable)
		}
		// Pin before the import: retention in the import could otherwise
		// delete an unpinned earlier safe point.
		n := *status.SafePoint
		if _, err := s.pinLocked(ctx, slugName, n, true); err != nil {
			if errors.Is(err, errPinsFull) {
				return 0, blocked(ProblemPinQuotaFull)
			}
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
	if imp.pinsFull {
		return 0, blocked(ProblemPinQuotaFull)
	}
	if imp.safePoint == 0 {
		return 0, fmt.Errorf("%w: the safe point was recorded but not imported; try again", ErrUnavailable)
	}
	return imp.safePoint, nil
}

// levelTimeout bounds telling a running board its public access level.
const levelTimeout = 10 * time.Second

// applyLevelLocked makes a running board apply the public access level,
// independent of the caller's context. A board that does not take a level
// other than edit is restarted: that closes every guest connection, and
// the board opens again with the stored level. Stopping always succeeds,
// so a level other than edit is in effect when it returns; the note says
// what else happened. The caller holds the flat lock, after the level
// committed.
func (s *Service) applyLevelLocked(ctx context.Context, slugName, level string) (note string) {
	in := s.liveBoard(slugName)
	if in == nil {
		return ""
	}
	ctx = context.WithoutCancel(ctx)
	lctx, cancel := context.WithTimeout(ctx, levelTimeout)
	defer cancel()
	err := s.publicBoardOf(in).SetPublicAccess(lctx, level)
	if err == nil {
		return ""
	}
	s.Event(ctx, slugName, "error", "visibility", fmt.Sprintf("the running board did not apply public access %q: %v", accessName(level), err), nil)
	if level == store.AccessEdit {
		// The board keeps refusing guests until it runs at edit.
		return "the running board did not take the new level yet, so guests cannot join until it restarts (get_logs has details)"
	}
	lf := s.state(slugName)
	old := lf.cur.Swap(nil)
	if old == nil || old.inst != in {
		// The board was replaced meanwhile; the new one opened with the
		// stored level.
		if old != nil {
			lf.cur.Store(old)
		}
		return ""
	}
	stopInstance(old.inst, "")
	if rerr := s.restart(lctx, slugName, old); rerr != nil {
		return "guest editing ended: the board was stopped, and it could not start again (" + rerr.Error() + ")"
	}
	s.Event(ctx, slugName, "warn", "visibility", "the board restarted to end guest editing", nil)
	return "the board restarted to end guest editing"
}

// repairLevelLocked applies the stored level to a running board again
// when that level is not edit, so repeating Stop guest editing finishes a
// change whose notification did not complete. The caller holds the flat
// lock.
func (s *Service) repairLevelLocked(ctx context.Context, f store.Flat) string {
	if f.PublicAccess == store.AccessEdit {
		return ""
	}
	return s.applyLevelLocked(ctx, f.Slug, f.PublicAccess)
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

// markSafePoint marks a board's safe point among revs while anyone can
// edit the board.
func (s *Service) markSafePoint(ctx context.Context, slugName string, st store.ContentState, revs []store.Revision) {
	if st.Type != contenttype.Board || st.SafePointRev == 0 {
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

// editQuotaProblems names the boards at anyone-can-edit that settings
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

// downgradeEditBoards sets every board at anyone-can-edit whose quotas
// the current settings no longer allow, after an offline configuration
// edit, back to view only, and records why (TECH 2.5).
func (s *Service) downgradeEditBoards(ctx context.Context) error {
	slugs, err := s.st.FlatsWithAccess(ctx, store.AccessEdit)
	if err != nil {
		return err
	}
	for _, slugName := range slugs {
		reason := ""
		st, err := s.st.ContentState(ctx, slugName)
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return err
		case err != nil || st.Type != contenttype.Board:
			reason = "it is not a board"
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
		ev := store.Event{Time: s.now(), Level: "warn", Kind: "visibility",
			Message: "guest editing stopped at startup because " + reason + "; the board stays Public, view only"}
		if err := s.st.SetPublicAccess(ctx, slugName, store.AccessEdit, store.AccessView, ev); err != nil {
			return fmt.Errorf("stop guest editing of %s: %w", slugName, err)
		}
		s.logf("flat %s: %s", slugName, ev.Message)
	}
	return nil
}

// withPublicAccess sets the trusted public access header of a Public board
// request; trustedAccess has removed any the client sent.
func withPublicAccess(r *http.Request, d *deployed, access string) *http.Request {
	if access == "" {
		return r
	}
	if _, ok := d.inst.(*board.Instance); ok {
		r.Header.Set(board.PublicAccessHeader, access)
	}
	return r
}
