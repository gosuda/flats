package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

const (
	dataUntouched = "The health check did not run. Live data was not changed."
	dataIsolated  = "The health check ran on a copy of the data. Live data was not changed."
)

// PendingApproval is returned when an action was recorded and not applied.
type PendingApproval struct {
	ActionResult
}

func (e *PendingApproval) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return ErrPending.Error()
}

func (e *PendingApproval) Unwrap() error { return ErrPending }

type publishParams struct {
	Revision    int    `json:"revision"`
	Hash        string `json:"hash"`
	BaseVersion int    `json:"base_version"`
	Visibility  string `json:"visibility"`
	Providers   string `json:"providers"`
}

type activateParams struct {
	Version      int    `json:"version"`
	Hash         string `json:"hash"`
	ExpectedLive int    `json:"expected_live"`
	Visibility   string `json:"visibility"`
	Providers    string `json:"providers"`
}

type rollbackParams struct {
	Hash         string `json:"hash"`
	Snapshot     string `json:"snapshot,omitempty"`
	SnapshotHash string `json:"snapshot_hash,omitempty"`
	Version      int    `json:"version"`
	ExpectedLive int    `json:"expected_live"`
	RestoreData  bool   `json:"restore_data"`
	Visibility   string `json:"visibility"`
	Providers    string `json:"providers"`
}

type visibilityParams struct {
	Visibility   string `json:"visibility"`
	From         string `json:"from"`
	ExpectedLive int    `json:"expected_live"`
	Providers    string `json:"providers"`
}

func (s *Service) contentDir(slugName string, v store.Version) string {
	if !v.Published && v.Revision > 0 {
		return s.draftRevDir(slugName, v.Revision)
	}
	return s.versionDir(slugName, v.Number)
}

func (s *Service) draftRevDir(slugName string, revision int) string {
	return filepath.Join(s.flatDir(slugName), "draft-revs", strconv.Itoa(revision))
}

func (s *Service) lifecycleNet() (LifecycleNet, bool) {
	if s.cfg.Lifecycle != nil {
		return s.cfg.Lifecycle, true
	}
	return nil, false
}

func (s *Service) providerKey(ctx context.Context, slugName string) (string, error) {
	ps, err := s.st.PermittedProviders(ctx, slugName)
	if err != nil {
		return "", err
	}
	slices.Sort(ps)
	key := strings.Join(ps, ",")
	if observer, ok := s.cfg.Lifecycle.(LifecycleObserver); ok {
		policy, err := observer.ExposurePolicy(ctx)
		if err != nil {
			return "", err
		}
		key += "\n" + policy
	}
	return key, nil
}

func (s *Service) permittedIDs(ctx context.Context, slugName string) ([]ProviderID, error) {
	ps, err := s.st.PermittedProviders(ctx, slugName)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderID, 0, len(ps))
	for _, p := range ps {
		out = append(out, ProviderID(p))
	}
	return out, nil
}

func (s *Service) requestFrozen(ctx context.Context, slugName, action string, params any, via Via, reason string) (ActionResult, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return ActionResult{}, err
	}
	sum := sha256.Sum256([]byte(slugName + "\n" + action + "\n" + string(raw)))
	key := hex.EncodeToString(sum[:])
	if existing, err := s.st.FindOpenApprovalByKey(ctx, key); err == nil {
		url := s.ApprovalURL(existing.ID)
		return ActionResult{Status: "pending_approval", Approval: &existing, ApprovalURL: url,
			Message: "This action is already waiting for the operator. Nothing changes until they approve it in the web console."}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return ActionResult{}, err
	}
	a := store.Approval{
		ID: "apr-" + slug.Random(12), Flat: slugName, Action: action, Params: raw,
		Status: "pending", Via: string(via), Reason: reason, RequestedAt: s.now(), IdempotencyKey: key,
	}
	if err := s.st.InsertApproval(ctx, a); err != nil {
		if existing, ferr := s.st.FindOpenApprovalByKey(ctx, key); ferr == nil {
			url := s.ApprovalURL(existing.ID)
			return ActionResult{Status: "pending_approval", Approval: &existing, ApprovalURL: url,
				Message: "This action is already waiting for the operator. Nothing changes until they approve it in the web console."}, nil
		}
		return ActionResult{}, err
	}
	url := s.ApprovalURL(a.ID)
	s.Event(ctx, slugName, "warn", "approval", fmt.Sprintf("%s requested via %s; waiting for the operator: %s", action, via, url), params)
	return ActionResult{Status: "pending_approval", Approval: &a, ApprovalURL: url,
		Message: "This action needs the operator's approval. Share the approval link with them; nothing changes until they approve it in the web console."}, nil
}

func pending(res ActionResult) (DeployResult, error) {
	return DeployResult{}, &PendingApproval{ActionResult: res}
}

func (s *Service) requestDeploy(ctx context.Context, slugName string, n int, via Via) (DeployResult, error) {
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	providers, err := s.providerKey(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	if n == 0 {
		d, err := s.st.GetDraft(ctx, slugName)
		if err != nil {
			return DeployResult{}, err
		}
		rev, err := s.st.GetDraftRevision(ctx, slugName, d.Revision)
		if err != nil {
			return DeployResult{}, err
		}
		if rev.Pruned {
			return DeployResult{}, fmt.Errorf("%w: draft revision %d was pruned", ErrConflict, rev.Revision)
		}
		res, err := s.requestFrozen(ctx, slugName, "publish", publishParams{
			Revision: d.Revision, Hash: rev.Hash, BaseVersion: f.LiveVersion,
			Visibility: string(f.Visibility.Canonical()), Providers: providers,
		}, via, "")
		if err != nil {
			return DeployResult{}, err
		}
		return pending(res)
	}
	v, err := s.st.GetVersion(ctx, slugName, n)
	if err != nil {
		return DeployResult{}, err
	}
	res, err := s.requestFrozen(ctx, slugName, "activate", activateParams{
		Version: n, Hash: v.Hash, ExpectedLive: f.LiveVersion,
		Visibility: string(f.Visibility.Canonical()), Providers: providers,
	}, via, "")
	if err != nil {
		return DeployResult{}, err
	}
	return pending(res)
}

func (s *Service) requestRollback(ctx context.Context, slugName string, to int, restoreData bool, via Via) (DeployResult, error) {
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	if f.LiveVersion == 0 {
		return DeployResult{}, ErrNotDeployed
	}
	if to == 0 {
		if to, err = s.previousLive(ctx, f); err != nil {
			return DeployResult{}, err
		}
	}
	if to == f.LiveVersion {
		return DeployResult{}, fmt.Errorf("%w: version %d is already live", ErrConflict, to)
	}
	v, err := s.st.GetVersion(ctx, slugName, to)
	if err != nil {
		return DeployResult{}, err
	}
	providers, err := s.providerKey(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	snapshot, shash := "", ""
	if restoreData {
		snapshot, err = s.liveSnapshot(slugName, f.LiveVersion)
		if err != nil {
			return DeployResult{}, err
		}
		shash, err = snapshotHash(filepath.Join(s.snapshotDir(slugName), snapshot))
		if err != nil {
			return DeployResult{}, err
		}
	}
	res, err := s.requestFrozen(ctx, slugName, "rollback", rollbackParams{
		Version: to, ExpectedLive: f.LiveVersion, RestoreData: restoreData, Hash: v.Hash, Snapshot: snapshot, SnapshotHash: shash,
		Visibility: string(f.Visibility.Canonical()), Providers: providers,
	}, via, "")
	if err != nil {
		return DeployResult{}, err
	}
	return pending(res)
}

func (s *Service) requestVisibility(ctx context.Context, slugName string, vis store.Visibility, via Via, reason string) (ActionResult, error) {
	if !vis.Valid() {
		return ActionResult{}, invalidf("unknown visibility %q (use private or public)", vis)
	}
	vis = vis.Canonical()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	if f.Visibility.Canonical() == vis {
		fv := s.view(ctx, f)
		return ActionResult{Status: "done", Flat: &fv, Notice: fv.PublicNotice, Message: "visibility unchanged"}, nil
	}
	if vis.Public() {
		n, err := s.st.CountPublished(ctx, slugName)
		if err != nil {
			return ActionResult{}, err
		}
		if f.LiveVersion == 0 && n == 0 {
			return ActionResult{}, fmt.Errorf("%w: publish a version before making this flat public", ErrConflict)
		}
		if err := s.publicAvailable(ctx, slugName); err != nil {
			return ActionResult{}, err
		}
	}
	providers, err := s.providerKey(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	res, err := s.requestFrozen(ctx, slugName, "set_visibility", visibilityParams{
		Visibility: string(vis), From: string(f.Visibility.Canonical()),
		ExpectedLive: f.LiveVersion, Providers: providers,
	}, via, reason)
	if err != nil {
		return ActionResult{}, err
	}
	if vis.Public() {
		res.Notice = PublicAccessNotice
	}
	return res, nil
}

func (s *Service) publicAvailable(ctx context.Context, slugName string) error {
	portal, err := s.st.ProviderPermitted(ctx, slugName, store.ProviderPortal)
	if err != nil {
		return err
	}
	funnel, err := s.st.ProviderPermitted(ctx, slugName, store.ProviderFunnel)
	if err != nil {
		return err
	}
	if _, ok := s.lifecycleNet(); ok && (portal || funnel) {
		return nil
	}
	if portal && s.cfg.Public != nil {
		return nil
	}
	if portal && s.cfg.Public == nil {
		return errPublicDisabled()
	}
	return fmt.Errorf("%w: %w: permit portal or tailscale-funnel before making this flat public", ErrConflict, ErrProviderNotPermitted)
}

// SetProviderPermission records an explicit non-local provider permission.
// Only the console may call it. It does not publish the flat or change visibility.
func (s *Service) SetProviderPermission(ctx context.Context, slugName, provider string, permitted bool, via Via) error {
	unlock := s.lock(slugName)
	defer unlock()
	if via != ViaConsole {
		return forbiddenf("only the operator can permit a network provider")
	}
	if s.cfg.ValidateOperatorDecision == nil {
		return forbiddenf("operator decision authority is not configured")
	}
	if err := s.cfg.ValidateOperatorDecision(ctx); err != nil {
		return fmt.Errorf("%w: operator decision: %v", ErrForbidden, err)
	}
	switch provider {
	case store.ProviderTailscale, store.ProviderFunnel, store.ProviderPortal:
	case store.ProviderLocal:
		return invalidf("local is always permitted")
	default:
		return invalidf("unknown provider %q", provider)
	}
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return err
	}
	if err := s.st.SetProviderPermission(ctx, slugName, provider, permitted); err != nil {
		return err
	}
	s.Event(ctx, slugName, "info", "provider", fmt.Sprintf("provider %s permitted=%t via %s", provider, permitted, via), nil)
	return nil
}

func (s *Service) resumeApplying(ctx context.Context) {
	as, err := s.st.ListApprovals(ctx, "applying")
	if err != nil {
		s.logf("resume approvals: %v", err)
		return
	}
	for _, a := range as {
		if a.AuthorizedAt == nil || a.DecidedBy == "" {
			_ = s.st.FinishApproval(ctx, a.ID, "failed", "legacy applying approval lacks validated operator decision; request fresh approval", s.now())
			continue
		}
		if _, err := s.finishApplying(ctx, a); err != nil {
			s.logf("resume approval %s: %v", a.ID, err)
		}
	}
}

func (s *Service) finishApplying(ctx context.Context, a store.Approval) (store.Approval, error) {
	var receipt ApprovalExecution
	if json.Unmarshal(a.ResultData, &receipt) == nil && receipt.Status == "applied" {
		receipt.Status = "approved"
		raw, _ := json.Marshal(receipt)
		if err := s.st.FinishApprovalData(ctx, a.ID, "approved", "visibility applied", raw, s.now()); err != nil {
			return a, err
		}
		return s.st.GetApproval(ctx, a.ID)
	}

	if dep, ok, err := s.st.DeploymentByApproval(ctx, a.ID); err != nil {
		return a, err
	} else if ok {
		msg := fmt.Sprintf("version %d is live (was %d)", dep.Version, dep.Previous)
		dto := ApprovalExecution{Status: "approved", DataImpact: "none", HealthData: "isolated_copy", LiveData: "untouched"}
		if v, err := s.st.GetVersion(ctx, a.Flat, dep.Version); err == nil && v.Kind == "server" {
			dto.DataImpact, dto.LiveData = "runtime_start", "runtime_may_write"
		}
		var params rollbackParams
		_ = json.Unmarshal(a.Params, &params)
		if params.RestoreData {
			dto.DataImpact, dto.LiveData = "restore_data", "restored"
		}
		raw, _ := json.Marshal(dto)
		if err := s.st.FinishApprovalData(ctx, a.ID, "approved", msg, raw, s.now()); err != nil {
			return a, err
		}
		return s.st.GetApproval(ctx, a.ID)
	}
	res, err := s.applyApproval(ctx, a)
	lifecyclePhase(ctx, "after_live_before_finalize", a.Flat, a.ID)
	status, result := "approved", res.Message
	if err != nil {
		status, result = "failed", err.Error()
	}
	dto := ApprovalExecution{Status: status, DataImpact: res.DataImpact, HealthData: res.HealthData, LiveData: res.LiveData}
	if dto.DataImpact == "" {
		dto.DataImpact, dto.HealthData, dto.LiveData = "none", "not_run", "untouched"
	}
	if err != nil {
		dto.FailureCode = failureCode(err)
		var de *DeployError
		if errors.As(err, &de) {
			dto.DataImpact, dto.HealthData, dto.LiveData = de.DataImpact, de.HealthData, de.LiveData
			if dto.DataImpact == "" {
				dto.DataImpact, dto.HealthData, dto.LiveData = "none", "isolated_copy", "untouched"
			}
		}
	}
	raw, _ := json.Marshal(dto)
	if derr := s.st.FinishApprovalData(ctx, a.ID, status, result, raw, s.now()); derr != nil {
		return a, derr
	}
	if a.Action != "delete" || err != nil {
		s.Event(ctx, a.Flat, "info", "approval", fmt.Sprintf("%s %s by the operator: %s", a.Action, status, result), nil)
	}
	out, gerr := s.st.GetApproval(ctx, a.ID)
	if gerr != nil {
		return out, gerr
	}
	return out, err
}

func (s *Service) applyApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	switch a.Action {
	case "delete":
		return s.applyDelete(ctx, a.Flat, ViaConsole, a.ID)
	case "set_visibility":
		return s.applyVisibilityApproval(ctx, a)
	case "publish":
		return s.applyPublishApproval(ctx, a)
	case "activate":
		return s.applyActivateApproval(ctx, a)
	case "rollback", "restore_data":
		return s.applyRollbackApproval(ctx, a)
	default:
		return ActionResult{}, fmt.Errorf("unknown action %q", a.Action)
	}
}

func mismatch(frozen, current, what string) error {
	return fmt.Errorf("%w: %w: %s changed after the request (approval has %s, current is %s)", ErrConflict, ErrStaleApproval, what, frozen, current)
}

func (s *Service) applyPublishApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	var p publishParams
	if err := json.Unmarshal(a.Params, &p); err != nil {
		return ActionResult{}, err
	}
	unlock := s.lock(a.Flat)
	defer unlock()
	f, err := s.st.GetFlat(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if f.LiveVersion != p.BaseVersion {
		return ActionResult{}, mismatch(strconv.Itoa(p.BaseVersion), strconv.Itoa(f.LiveVersion), "the live version")
	}
	if string(f.Visibility.Canonical()) != p.Visibility {
		return ActionResult{}, mismatch(p.Visibility, string(f.Visibility.Canonical()), "visibility")
	}
	providers, err := s.providerKey(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if providers != p.Providers {
		return ActionResult{}, mismatch(p.Providers, providers, "provider permissions")
	}
	draft, err := s.st.GetDraft(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if draft.Revision != p.Revision || draft.Hash != p.Hash {
		return ActionResult{}, mismatch(fmt.Sprint(p.Revision)+":"+p.Hash, fmt.Sprint(draft.Revision)+":"+draft.Hash, "draft")
	}
	rev, err := s.st.GetDraftRevision(ctx, a.Flat, p.Revision)
	if err != nil {
		return ActionResult{}, err
	}
	if rev.Hash != p.Hash || rev.Pruned {
		return ActionResult{}, fmt.Errorf("%w: draft revision %d no longer matches the approved snapshot", ErrConflict, p.Revision)
	}
	if f.LiveVersion > 0 {
		if v, err := s.st.GetVersion(ctx, f.Slug, f.LiveVersion); err == nil && v.Hash == rev.Hash {
			return ActionResult{}, fmt.Errorf("%w: %w", ErrConflict, ErrUnchangedContent)
		}
	}
	if err := s.verifyContent(a.Flat, versionFromRevision(rev)); err != nil {
		return ActionResult{}, err
	}
	res, err := s.publishRevision(ctx, f, rev, a.ID)
	if err != nil {
		return ActionResult{}, err
	}
	return ActionResult{Status: "done", Flat: &res.Flat, Message: fmt.Sprintf("published version %d", res.Version), DataImpact: res.DataImpact, HealthData: res.HealthData, LiveData: res.LiveData}, nil
}

func (s *Service) publishRevision(ctx context.Context, f store.Flat, rev store.DraftRevision, approvalID string) (DeployResult, error) {
	candidate := versionFromRevision(rev)
	lifecyclePhase(ctx, "before_health", f.Slug, approvalID)
	h, err := s.checkIsolated(ctx, f, candidate)
	if err != nil {
		return DeployResult{}, err
	}
	lifecyclePhase(ctx, "after_health", f.Slug, approvalID)
	n, err := s.st.NextVersionNumber(ctx, f.Slug)
	if err != nil {
		return DeployResult{}, err
	}
	lifecyclePhase(ctx, "after_allocation_before_live", f.Slug, approvalID)
	dir := s.versionDir(f.Slug, n)
	if err := os.RemoveAll(dir); err != nil {
		return DeployResult{}, err
	}
	if err := copyTree(s.draftRevDir(f.Slug, rev.Revision), dir); err != nil {
		os.RemoveAll(dir)
		return DeployResult{}, err
	}
	v := candidate
	v.Number = n
	v.Published = true
	v.Role = "published"
	v.Revision = 0
	v.CreatedAt = s.now()
	if snap, serr := s.snapshotDB(f.Slug, n, f.LiveVersion); serr != nil {
		s.Event(ctx, f.Slug, "warn", "snapshot", "could not snapshot the database before publish: "+serr.Error(), nil)
	} else if snap != "" {
		s.Event(ctx, f.Slug, "info", "snapshot", fmt.Sprintf("saved database snapshot %s before publishing version %d", snap, n), nil)
	}
	d, err := s.build(ctx, f.Slug, v, s.dataDirOf(f.Slug))
	if err != nil {
		os.RemoveAll(dir)
		return DeployResult{}, &DeployError{Version: n, Previous: f.LiveVersion, Cause: err, Data: "Runtime startup used live data and may have changed it.", DataImpact: "runtime_start", HealthData: "isolated_copy", LiveData: "runtime_may_write"}
	}
	res, err := s.activate(ctx, f, d, h, "publish", ViaConsole, approvalID, time.Now())
	if err != nil {
		if d.inst != nil {
			d.inst.Stop()
		}
		os.RemoveAll(dir)
		return DeployResult{}, err
	}
	if cur, err := s.st.GetDraft(ctx, f.Slug); err == nil && cur.Revision == rev.Revision && cur.Hash == rev.Hash {
		cur.Dirty = false
		cur.BaseVersion = n
		cur.UpdatedAt = s.now()
		_ = s.st.PutDraft(ctx, cur)
	}
	return res, nil
}

func versionFromRevision(r store.DraftRevision) store.Version {
	return store.Version{
		Flat: r.Flat, Revision: r.Revision, Hash: r.Hash, Size: r.Size, Files: r.Files,
		Kind: r.Kind, Manifest: r.Manifest, GitSHA: r.GitSHA, GitDirty: r.GitDirty,
		Message: r.Message, CreatedAt: r.CreatedAt, Screenshot: r.Screenshot, Pruned: r.Pruned,
		Published: false, Role: "draft",
	}
}

func (s *Service) applyActivateApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	var p activateParams
	if err := json.Unmarshal(a.Params, &p); err != nil {
		return ActionResult{}, err
	}
	unlock := s.lock(a.Flat)
	defer unlock()
	f, err := s.st.GetFlat(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if f.LiveVersion != p.ExpectedLive {
		return ActionResult{}, mismatch(strconv.Itoa(p.ExpectedLive), strconv.Itoa(f.LiveVersion), "the live version")
	}
	if string(f.Visibility.Canonical()) != p.Visibility {
		return ActionResult{}, mismatch(p.Visibility, string(f.Visibility.Canonical()), "visibility")
	}
	providers, err := s.providerKey(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if providers != p.Providers {
		return ActionResult{}, mismatch(p.Providers, providers, "provider permissions")
	}
	v, err := s.st.GetVersion(ctx, a.Flat, p.Version)
	if err != nil {
		return ActionResult{}, err
	}
	if v.Hash != p.Hash {
		return ActionResult{}, fmt.Errorf("%w: version %d no longer matches the approved snapshot", ErrConflict, p.Version)
	}
	if err := s.verifyContent(a.Flat, v); err != nil {
		return ActionResult{}, err
	}
	kind := "deploy"
	if f.LiveVersion == p.Version {
		kind = "redeploy"
	}
	res, err := s.deployLocked(ctx, f, p.Version, kind, ViaConsole, a.ID)
	if err != nil {
		return ActionResult{}, err
	}
	return ActionResult{Status: "done", Flat: &res.Flat, Message: fmt.Sprintf("version %d is live", res.Version), DataImpact: res.DataImpact, HealthData: res.HealthData, LiveData: res.LiveData}, nil
}

func (s *Service) applyRollbackApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	var p rollbackParams
	if err := json.Unmarshal(a.Params, &p); err != nil {
		return ActionResult{}, err
	}
	unlock := s.lock(a.Flat)
	defer unlock()
	f, err := s.st.GetFlat(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if f.LiveVersion != p.ExpectedLive {
		return ActionResult{}, mismatch(strconv.Itoa(p.ExpectedLive), strconv.Itoa(f.LiveVersion), "the live version")
	}
	if string(f.Visibility.Canonical()) != p.Visibility {
		return ActionResult{}, mismatch(p.Visibility, string(f.Visibility.Canonical()), "visibility")
	}
	providers, err := s.providerKey(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if providers != p.Providers {
		return ActionResult{}, mismatch(p.Providers, providers, "provider permissions")
	}
	v, err := s.st.GetVersion(ctx, a.Flat, p.Version)
	if err != nil {
		return ActionResult{}, err
	}
	if v.Hash != p.Hash {
		return ActionResult{}, mismatch(p.Hash, v.Hash, "version hash")
	}
	if err := s.verifyContent(a.Flat, v); err != nil {
		return ActionResult{}, err
	}
	var res DeployResult
	if p.RestoreData {
		hash, hashErr := snapshotHash(filepath.Join(s.snapshotDir(a.Flat), p.Snapshot))
		if hashErr != nil {
			return ActionResult{}, hashErr
		}
		if hash != p.SnapshotHash {
			return ActionResult{}, mismatch(p.SnapshotHash, hash, "restore snapshot")
		}
		kind := "rollback"
		if a.Action == "restore_data" {
			kind = "restore"
		}
		res, err = s.restoreLocked(ctx, f, p.Snapshot, p.Version, kind, ViaConsole, a.ID)
	} else {
		res, err = s.rollbackLocked(ctx, f, p.Version, false, ViaConsole, a.ID)
	}
	if err != nil {
		return ActionResult{}, err
	}
	msg := fmt.Sprintf("rolled back to version %d", res.Version)
	if p.RestoreData {
		msg += " and restored its database snapshot"
	}
	return ActionResult{Status: "done", Flat: &res.Flat, Message: msg, DataImpact: res.DataImpact, HealthData: res.HealthData, LiveData: res.LiveData}, nil
}

func (s *Service) applyVisibilityApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	var p visibilityParams
	if err := json.Unmarshal(a.Params, &p); err != nil {
		return ActionResult{}, err
	}
	target := store.Visibility(p.Visibility).Canonical()
	if !target.Valid() || (target != store.Private && target != store.Public) {
		return ActionResult{}, invalidf("unknown visibility %q", p.Visibility)
	}
	unlock := s.lock(a.Flat)
	defer unlock()
	f, err := s.st.GetFlat(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if string(f.Visibility.Canonical()) != p.From {
		return ActionResult{}, mismatch(p.From, string(f.Visibility.Canonical()), "visibility")
	}
	if f.LiveVersion != p.ExpectedLive {
		return ActionResult{}, mismatch(strconv.Itoa(p.ExpectedLive), strconv.Itoa(f.LiveVersion), "the live version")
	}
	providers, err := s.providerKey(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if providers != p.Providers {
		return ActionResult{}, mismatch(p.Providers, providers, "provider permissions")
	}
	if f.Visibility.Canonical() == target {
		fv := s.view(ctx, f)
		return ActionResult{Status: "done", Flat: &fv, Message: "visibility unchanged"}, nil
	}
	if target.Public() {
		n, err := s.st.CountPublished(ctx, a.Flat)
		if err != nil {
			return ActionResult{}, err
		}
		if f.LiveVersion == 0 && n == 0 {
			return ActionResult{}, fmt.Errorf("%w: publish a version before making this flat public", ErrConflict)
		}
		if err := s.publicAvailable(ctx, a.Flat); err != nil {
			return ActionResult{}, err
		}
		next := f
		next.Visibility = target
		if err := s.ensureExposure(ctx, next); err != nil {
			_ = s.stopPublicConfirmed(ctx, a.Flat)
			_ = s.ensureExposure(ctx, f)
			return ActionResult{}, err
		}
		next.UpdatedAt = s.now()
		raw, _ := json.Marshal(ApprovalExecution{Status: "applied", DataImpact: "none", HealthData: "not_run", LiveData: "untouched"})
		if err := s.st.CommitVisibility(ctx, next, f.Visibility, a.ID, raw); err != nil {
			_ = s.ensureExposure(ctx, f)
			return ActionResult{}, err
		}
		s.Event(ctx, a.Flat, "info", "visibility", fmt.Sprintf("visibility %s -> %s", p.From, target), nil)
		fv := s.view(ctx, next)
		return ActionResult{Status: "done", Flat: &fv, Notice: fv.PublicNotice, Message: "visibility changed to " + string(target)}, nil
	}
	if err := s.stopPublicConfirmed(ctx, a.Flat); err != nil {
		return ActionResult{}, err
	}
	from := f.Visibility
	f.Visibility = store.Private
	f.UpdatedAt = s.now()
	raw, _ := json.Marshal(ApprovalExecution{Status: "applied", DataImpact: "none", HealthData: "not_run", LiveData: "untouched"})
	if err := s.st.CommitVisibility(ctx, f, from, a.ID, raw); err != nil {
		return ActionResult{}, err
	}
	s.stopPublicRedirects(a.Flat)
	if err := s.ensureExposure(ctx, f); err != nil {
		s.Event(ctx, a.Flat, "error", "exposure", err.Error(), nil)
		return ActionResult{}, fmt.Errorf("%w: private visibility is saved but exposure did not match it: %v", ErrConflict, err)
	}
	s.Event(ctx, a.Flat, "info", "visibility", fmt.Sprintf("visibility %s -> private", p.From), nil)
	fv := s.view(ctx, f)
	return ActionResult{Status: "done", Flat: &fv, Message: "visibility changed to private"}, nil
}

func (s *Service) stopPublicConfirmed(ctx context.Context, slugName string) error {
	if ln, ok := s.lifecycleNet(); ok {
		res, err := ln.StopPublicRoutes(ctx, slugName)
		if err != nil {
			return fmt.Errorf("%w: stop public routes: %v", ErrConflict, err)
		}
		if len(res.Unconfirmed) > 0 {
			names := make([]string, 0, len(res.Unconfirmed))
			for _, p := range res.Unconfirmed {
				names = append(names, string(p))
			}
			return fmt.Errorf("%w: %w: public routes still reachable: %s", ErrConflict, ErrPublicStopUnconfirmed, strings.Join(names, ", "))
		}
		s.state(slugName).publicServed = false
		return nil
	}
	lf := s.state(slugName)
	if lf.publicServed && s.cfg.Public != nil {
		if err := s.cfg.Public.Stop(slugName); err != nil {
			return fmt.Errorf("%w: stop public exposure: %v", ErrConflict, err)
		}
		lf.publicServed = false
	}
	return nil
}

func (s *Service) ensureLifecycle(ctx context.Context, f store.Flat, ln LifecycleNet) error {
	lf := s.state(f.Slug)
	if lf.cur.Load() == nil {
		return nil
	}
	permitted, err := s.permittedIDs(ctx, f.Slug)
	if err != nil {
		return err
	}
	if !f.Visibility.Public() {
		permitted = privateProviders(permitted)
	}
	if f.Visibility.Public() {
		if _, err := ln.ServeExposure(ctx, ExposureRequest{Slug: f.Slug, Host: f.Slug, Visibility: "private", Audience: AudienceCurrent, Handler: s.siteHandler(f.Slug, false), Permitted: privateProviders(permitted)}); err != nil {
			return fmt.Errorf("private exposure: %w", err)
		}
	}
	res, err := ln.ServeExposure(ctx, ExposureRequest{
		Slug: f.Slug, Host: f.Slug, Visibility: string(f.Visibility.Canonical()),
		Audience: AudienceCurrent, Handler: s.siteHandler(f.Slug, f.Visibility.Public()),
		Permitted: permitted,
	})
	if err != nil {
		return fmt.Errorf("exposure: %w", err)
	}
	if f.Visibility.Public() {
		ready := false
		for _, ep := range res.Endpoints {
			if (ep.Provider == ProviderFunnel || ep.Provider == ProviderPortal) && ep.State == "ready" {
				ready = true
			}
		}
		if !ready {
			return fmt.Errorf("%w: no public endpoint is ready", ErrProviderNotReady)
		}
	}
	lf.privateServed = true
	lf.publicServed = f.Visibility.Public()
	return nil
}

func (s *Service) checkIsolated(ctx context.Context, f store.Flat, v store.Version) (HealthResult, error) {
	scratch := filepath.Join(s.flatDir(f.Slug), "health-check")
	_ = os.RemoveAll(scratch)
	defer os.RemoveAll(scratch)
	if v.Kind == "server" {
		if err := snapshotData(s.dataDirOf(f.Slug), scratch); err != nil {
			return HealthResult{}, &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: fmt.Errorf("copy data for health check: %w", err), Data: dataUntouched}
		}
	}
	d, err := s.build(ctx, f.Slug, v, scratch)
	if err != nil {
		return HealthResult{}, &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: err, Data: dataUntouched}
	}
	h := healthCheck(d.handler, manifestOf(v).Health, d.redact)
	if d.inst != nil {
		d.inst.Stop()
	}
	what := fmt.Sprintf("version %d", v.Number)
	if !v.Published && v.Revision > 0 {
		what = fmt.Sprintf("draft revision %d", v.Revision)
	}
	s.Event(ctx, f.Slug, map[bool]string{true: "info", false: "error"}[h.OK], "health",
		fmt.Sprintf("health check of %s: GET %s -> %d in %dms %s", what, h.Path, h.Status, h.Millis, h.Error), h)
	if !h.OK {
		return h, &DeployError{Version: v.Number, Previous: f.LiveVersion, Health: h, Data: dataIsolated}
	}
	return h, nil
}

func (s *Service) saveDraftLocked(ctx context.Context, slugName string, files []bundle.File, meta SaveMeta, via Via) (store.Version, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return store.Version{}, err
	}
	next, err := s.st.NextDraftRevision(ctx, slugName)
	if err != nil {
		return store.Version{}, err
	}
	if meta.CheckRevision {
		current := 0
		if d, err := s.st.GetDraft(ctx, slugName); err == nil {
			current = d.Revision
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.Version{}, err
		}
		if meta.ExpectedRevision != current {
			return store.Version{}, fmt.Errorf("%w: draft revision is %d", ErrConflict, current)
		}
	}
	dir := s.draftRevDir(slugName, next)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return store.Version{}, err
	}
	res, err := bundle.Write(files, dir)
	if err != nil {
		os.RemoveAll(dir)
		return store.Version{}, err
	}
	man, _ := json.Marshal(res.Manifest)
	now := s.now()
	rev := store.DraftRevision{
		Flat: slugName, Revision: next, Hash: res.Hash, Size: res.Size, Files: res.Files,
		Kind: res.Manifest.Kind, Manifest: man, GitSHA: meta.GitSHA, GitDirty: meta.GitDirty,
		Message: meta.Message, Screenshot: res.Manifest.Screenshot, CreatedAt: now,
	}
	if err := s.st.PutDraftRevision(ctx, rev); err != nil {
		os.RemoveAll(dir)
		return store.Version{}, err
	}
	dirty := true
	if f.LiveVersion > 0 {
		if live, err := s.st.GetVersion(ctx, slugName, f.LiveVersion); err == nil && live.Hash == res.Hash {
			dirty = false
		}
	}
	created := now
	if cur, err := s.st.GetDraft(ctx, slugName); err == nil {
		created = cur.CreatedAt
	}
	if err := s.st.PutDraft(ctx, store.Draft{
		Flat: slugName, Revision: next, Hash: res.Hash, BaseVersion: f.LiveVersion, Dirty: dirty,
		Size: res.Size, Files: res.Files, Kind: res.Manifest.Kind, Manifest: man,
		GitSHA: meta.GitSHA, GitDirty: meta.GitDirty, Message: meta.Message,
		UpdatedAt: now, CreatedAt: created,
	}); err != nil {
		return store.Version{}, err
	}
	if res.Manifest.Name != "" && f.Name == slugName {
		f.Name, f.UpdatedAt = res.Manifest.Name, now
		_ = s.st.UpdateFlat(ctx, f)
	}
	_ = s.st.Touch(ctx, slugName, now)
	s.Event(ctx, slugName, "info", "draft", fmt.Sprintf("saved draft revision %d (%d files, %d bytes) via %s", next, res.Files, res.Size, via), map[string]any{"revision": next, "hash": res.Hash, "git_sha": meta.GitSHA})
	s.pruneDraftRevisions(ctx, slugName, next)
	return versionFromRevision(rev), nil
}

func (s *Service) pruneDraftRevisions(ctx context.Context, slugName string, keep int) {
	pinned := map[int]bool{keep: true}
	for _, status := range []string{"pending", "applying"} {
		as, err := s.st.ListApprovals(ctx, status)
		if err != nil {
			return
		}
		for _, a := range as {
			if a.Flat != slugName {
				continue
			}
			var p struct {
				Revision int `json:"revision"`
			}
			_ = json.Unmarshal(a.Params, &p)
			if p.Revision > 0 {
				pinned[p.Revision] = true
			}
		}
	}
	if ps, err := s.st.ListPreviews(ctx, slugName); err == nil {
		for _, p := range ps {
			if p.Target == "draft" {
				pinned[p.Revision] = true
			}
		}
	} else {
		return
	}
	revs, err := s.st.ListDraftRevisions(ctx, slugName)
	if err != nil {
		return
	}
	for _, r := range revs {
		if r.Pruned || pinned[r.Revision] {
			continue
		}
		if err := os.RemoveAll(s.draftRevDir(slugName, r.Revision)); err == nil {
			_ = s.st.MarkDraftRevisionPruned(ctx, slugName, r.Revision)
		}
	}
}

func (s *Service) openDraftPreviewLocked(ctx context.Context, slugName string) (PreviewView, error) {
	d, err := s.st.GetDraft(ctx, slugName)
	if err != nil {
		return PreviewView{}, err
	}
	rev, err := s.st.GetDraftRevision(ctx, slugName, d.Revision)
	if err != nil {
		return PreviewView{}, err
	}
	if rev.Pruned {
		return PreviewView{}, fmt.Errorf("%w: draft revision %d was pruned", ErrConflict, rev.Revision)
	}
	if ps, err := s.st.ListPreviews(ctx, slugName); err != nil {
		return PreviewView{}, err
	} else if len(ps) >= maxPreviews {
		return PreviewView{}, fmt.Errorf("%w: %d previews are already open; close one first", ErrConflict, len(ps))
	}
	host := slug.PreviewHost(slugName)
	dataDir := filepath.Join(s.flatDir(slugName), "previews", host)
	if rev.Kind == "server" {
		if err := snapshotData(s.dataDirOf(slugName), dataDir); err != nil {
			os.RemoveAll(dataDir)
			return PreviewView{}, fmt.Errorf("copy live data for preview: %w", err)
		}
	}
	built, err := s.build(ctx, slugName, versionFromRevision(rev), dataDir)
	if err != nil {
		os.RemoveAll(dataDir)
		return PreviewView{}, err
	}
	p := &preview{host: host, flat: slugName, version: 0, handler: built.handler, inst: built.inst, dataDir: dataDir}
	now := s.now()
	p.last.Store(now.UnixMilli())
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.last.Store(s.now().UnixMilli())
		w.Header().Set("X-Robots-Tag", "noindex")
		p.handler.ServeHTTP(w, r)
	})
	stop := func() {
		if built.inst != nil {
			built.inst.Stop()
		}
		os.RemoveAll(dataDir)
	}
	url, err := s.servePreview(ctx, slugName, host, h)
	if err != nil {
		stop()
		return PreviewView{}, err
	}
	rec := store.Preview{Host: host, Flat: slugName, Version: 0, CreatedAt: now, LastAccess: now, Target: "draft", Revision: rev.Revision}
	if err := s.st.InsertPreview(ctx, rec); err != nil {
		_ = s.stopPreviewExposure(ctx, host)
		stop()
		return PreviewView{}, err
	}
	s.mu.Lock()
	s.prevs[host] = p
	s.mu.Unlock()
	s.Event(ctx, slugName, "info", "preview", fmt.Sprintf("preview of draft revision %d at %s", rev.Revision, url), nil)
	pv := PreviewView{Preview: rec, URL: url, ExpiresAt: now.Add(s.previewTTL())}
	pv.State, pv.Detail = s.hostState(host)
	return pv, nil
}

func (s *Service) servePreview(ctx context.Context, slugName, host string, h http.Handler) (string, error) {
	if ln, ok := s.lifecycleNet(); ok {
		permitted, err := s.permittedIDs(ctx, slugName)
		if err != nil {
			return "", err
		}
		res, err := ln.ServeExposure(ctx, ExposureRequest{
			Slug: slugName, Host: host, Visibility: string(store.Private),
			Audience: AudienceDraft, Handler: h, Ephemeral: true, Permitted: privateProviders(permitted),
		})
		if err != nil {
			return "", err
		}
		for _, ep := range res.Endpoints {
			if ep.URL != "" {
				return ep.URL, nil
			}
		}
		return s.cfg.Private.URL(host), nil
	}
	return s.cfg.Private.Serve(ctx, host, h, true)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func (s *Service) migrateLegacyDrafts(ctx context.Context) error {
	vs, err := s.st.UnpublishedVersions(ctx)
	if err != nil {
		return err
	}
	for _, v := range vs {
		revs, err := s.st.ListDraftRevisions(ctx, v.Flat)
		if err != nil {
			return err
		}
		var rev store.DraftRevision
		for _, r := range revs {
			if r.LegacyNumber == v.Number {
				rev = r
				break
			}
		}
		if rev.Revision == 0 {
			next, err := s.st.NextDraftRevision(ctx, v.Flat)
			if err != nil {
				return err
			}
			rev = store.DraftRevision{Flat: v.Flat, Revision: next, Hash: v.Hash, Size: v.Size, Files: v.Files, Kind: v.Kind, Manifest: v.Manifest, GitSHA: v.GitSHA, GitDirty: v.GitDirty, Message: v.Message, Screenshot: v.Screenshot, CreatedAt: v.CreatedAt, Pruned: v.Pruned, LegacyNumber: v.Number}
			// Keep source bytes until both immutable metadata and mutable pointer commit.
			dst := s.draftRevDir(v.Flat, next)
			if !v.Pruned {
				if _, err := os.Stat(s.versionDir(v.Flat, v.Number)); err == nil {
					if err := os.RemoveAll(dst); err != nil {
						return err
					}
					if err := copyTree(s.versionDir(v.Flat, v.Number), dst); err != nil {
						return err
					}
				} else if os.IsNotExist(err) {
					rev.Pruned = true
				} else {
					return err
				}
			}
			if err := s.st.PutDraftRevision(ctx, rev); err != nil {
				return err
			}
		}
		if err := s.st.AdoptLegacyDraft(ctx, rev); err != nil {
			return err
		}
		// Originals are retained as migration evidence until a successful publish
		// reuses the number; the new current snapshot has its own immutable copy.
	}
	return nil
}

func privateProviders(ids []ProviderID) []ProviderID {
	out := []ProviderID{ProviderLocal}
	for _, id := range ids {
		if id == ProviderTailscale {
			out = append(out, id)
		}
	}
	return out
}

var ErrStaleApproval = errors.New("stale approval")
var ErrProviderNotReady = errors.New("provider not ready")
var ErrPublicStopUnconfirmed = errors.New("public stop unconfirmed")

// ApprovalExecution is persisted as Approval.result_data for transport serialization.
type ApprovalExecution struct {
	Status      string `json:"status"`
	FailureCode string `json:"failure_code,omitempty"`
	DataImpact  string `json:"data_impact"`
	HealthData  string `json:"health_data"`
	LiveData    string `json:"live_data"`
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, ErrStaleApproval):
		return "stale_approval"
	case errors.Is(err, ErrProviderNotPermitted):
		return "provider_not_permitted"
	case errors.Is(err, ErrUnavailable):
		return "provider_unavailable"
	case errors.Is(err, ErrUnchangedContent):
		return "unchanged_content"
	case errors.Is(err, ErrProviderNotReady):
		return "provider_not_ready"
	case errors.Is(err, ErrPublicStopUnconfirmed):
		return "public_stop_unconfirmed"
	}
	var de *DeployError
	if errors.As(err, &de) {
		if de.Cause != nil {
			return "runtime_start_failed"
		}
		return "health_check_failed"
	}
	return "apply_failed"
}

// GetDraft returns the current mutable Draft pointer.
func (s *Service) GetDraft(ctx context.Context, flat string) (store.Draft, error) {
	return s.st.GetDraft(ctx, flat)
}

// Publish requests the exact current revision; zero selects current.
func (s *Service) Publish(ctx context.Context, flat string, revision int, hash string, via Via) (ActionResult, error) {
	unlock := s.lock(flat)
	defer unlock()
	f, err := s.st.GetFlat(ctx, flat)
	if err != nil {
		return ActionResult{}, err
	}
	d, err := s.st.GetDraft(ctx, flat)
	if err != nil {
		return ActionResult{}, err
	}
	if revision != 0 && revision != d.Revision {
		return ActionResult{}, fmt.Errorf("%w: %w: draft revision changed", ErrConflict, ErrStaleApproval)
	}
	if hash != "" && hash != d.Hash {
		return ActionResult{}, fmt.Errorf("%w: %w: draft hash changed", ErrConflict, ErrStaleApproval)
	}
	key, err := s.providerKey(ctx, flat)
	if err != nil {
		return ActionResult{}, err
	}
	return s.requestFrozen(ctx, flat, "publish", publishParams{Revision: d.Revision, Hash: d.Hash, BaseVersion: f.LiveVersion, Visibility: string(f.Visibility.Canonical()), Providers: key}, via, "")
}

// SaveDraft is the explicit Draft save facade with optimistic revision checking.
func (s *Service) SaveDraft(ctx context.Context, flat string, files []bundle.File, meta SaveMeta, expectedRevision int, via Via) (store.Version, error) {
	meta.CheckRevision = true
	meta.ExpectedRevision = expectedRevision
	return s.SaveVersion(ctx, flat, files, meta, via)
}

// RequestPublish is the pending-action facade used by transports.
func (s *Service) RequestPublish(ctx context.Context, flat string, revision int, hash string, via Via) (ActionResult, error) {
	return s.Publish(ctx, flat, revision, hash, via)
}

var ErrUnchangedContent = errors.New("content already published")

func snapshotHash(path string) (string, error) {
	h := sha256.New()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h.Write(b)
	err = filepath.Walk(path+".data", func(p string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(path+".data", p)
			h.Write([]byte(rel))
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) stopPreviewExposure(ctx context.Context, host string) error {
	if n, ok := s.cfg.Lifecycle.(LifecyclePreviewNet); ok {
		return n.StopExposure(ctx, host)
	}
	return s.cfg.Private.Stop(host)
}
func (s *Service) verifyContent(flat string, v store.Version) error {
	files, err := bundle.FromDir(s.contentDir(flat, v), bundle.Limits{MaxBytes: max(v.Size+1, s.UploadLimit())})
	if err != nil {
		return err
	}
	if bundle.Hash(files) != v.Hash {
		return fmt.Errorf("%w: %w: immutable content hash changed", ErrConflict, ErrStaleApproval)
	}
	return nil
}

// restorePreviews retains persisted Private targets and their isolated data.
func (s *Service) restorePreviews(ctx context.Context) {
	ps, err := s.st.ListPreviews(ctx, "")
	if err != nil {
		s.logf("restore previews: %v", err)
		return
	}
	for _, p := range ps {
		if !p.LastAccess.Add(s.previewTTL()).After(s.now()) {
			_ = s.st.DeletePreview(ctx, p.Host)
			continue
		}
		var v store.Version
		if p.Target == "draft" {
			r, e := s.st.GetDraftRevision(ctx, p.Flat, p.Revision)
			err = e
			v = versionFromRevision(r)
		} else {
			v, err = s.st.GetVersion(ctx, p.Flat, p.Version)
		}
		if err != nil || v.Pruned {
			s.Event(ctx, p.Flat, "error", "preview", "preview target unavailable after restart", nil)
			continue
		}
		data := filepath.Join(s.flatDir(p.Flat), "previews", p.Host)
		built, err := s.build(ctx, p.Flat, v, data)
		if err != nil {
			s.Event(ctx, p.Flat, "error", "preview", err.Error(), nil)
			continue
		}
		prev := &preview{host: p.Host, flat: p.Flat, version: p.Version, handler: built.handler, inst: built.inst, dataDir: data}
		prev.last.Store(p.LastAccess.UnixMilli())
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			prev.last.Store(s.now().UnixMilli())
			w.Header().Set("X-Robots-Tag", "noindex")
			prev.handler.ServeHTTP(w, r)
		})
		if _, err := s.servePreview(ctx, p.Flat, p.Host, handler); err != nil {
			if built.inst != nil {
				built.inst.Stop()
			}
			s.Event(ctx, p.Flat, "error", "preview", err.Error(), nil)
			continue
		}
		s.mu.Lock()
		s.prevs[p.Host] = prev
		s.mu.Unlock()
	}
}
