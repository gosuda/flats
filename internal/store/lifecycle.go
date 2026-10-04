package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Provider identifiers stored in flat_providers.
const (
	ProviderLocal     = "local"
	ProviderTailscale = "tailscale"
	ProviderFunnel    = "tailscale-funnel"
	ProviderPortal    = "portal"
)

// Draft is the single mutable working copy of a flat.
type Draft struct {
	Flat        string          `json:"flat"`
	Revision    int             `json:"revision"`
	Hash        string          `json:"hash"`
	BaseVersion int             `json:"base_version"`
	Dirty       bool            `json:"dirty"`
	Size        int64           `json:"size"`
	Files       int             `json:"files"`
	Kind        string          `json:"kind"`
	Manifest    json.RawMessage `json:"manifest"`
	GitSHA      string          `json:"git_sha,omitempty"`
	GitDirty    bool            `json:"git_dirty"`
	Message     string          `json:"message,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
	CreatedAt   time.Time       `json:"created_at"`
}

// DraftRevision is one immutable snapshot of a draft save.
type DraftRevision struct {
	Flat         string          `json:"flat"`
	Revision     int             `json:"revision"`
	Hash         string          `json:"hash"`
	Size         int64           `json:"size"`
	Files        int             `json:"files"`
	Kind         string          `json:"kind"`
	Manifest     json.RawMessage `json:"manifest"`
	GitSHA       string          `json:"git_sha,omitempty"`
	GitDirty     bool            `json:"git_dirty"`
	Message      string          `json:"message,omitempty"`
	Screenshot   string          `json:"screenshot,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	Pruned       bool            `json:"pruned"`
	LegacyNumber int             `json:"legacy_number,omitempty"`
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// migrateLifecycle brings a v1 database to the draft/published split.
// It is idempotent: columns and tables that already exist are left in place.
func migrateLifecycle(db *sql.DB) error {
	if err := addColumn(db, "versions", "published", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumn(db, "approvals", "idempotency_key", "TEXT"); err != nil {
		return err
	}
	if err := addColumn(db, "deployments", "approval_id", "TEXT"); err != nil {
		return err
	}
	if err := addColumn(db, "previews", "target", "TEXT NOT NULL DEFAULT 'version'"); err != nil {
		return err
	}
	if err := addColumn(db, "previews", "revision", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS approvals_open_key ON approvals(idempotency_key) WHERE idempotency_key IS NOT NULL AND status IN ('pending','applying')`); err != nil {
		return err
	}
	// A version that was live or named by a deployment is published history.
	// Numbers and files stay. Saved-but-never-deployed rows stay published=0
	// until core moves them into draft revisions.
	if _, err := db.Exec(`UPDATE versions SET published=1
		WHERE published=0 AND (
			EXISTS (SELECT 1 FROM flats f WHERE f.slug=versions.flat AND f.live_version=versions.number AND f.live_version>0)
			OR EXISTS (SELECT 1 FROM deployments d WHERE d.flat=versions.flat AND d.version=versions.number)
			OR EXISTS (SELECT 1 FROM deployments d WHERE d.flat=versions.flat AND d.previous=versions.number AND d.previous>0)
		)`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE flats SET visibility='public' WHERE visibility IN ('public-listed','public-unlisted')`); err != nil {
		return err
	}
	// Unpublished flats are not public. A public flag with nothing activated
	// was a reservation the new contract does not keep.
	if _, err := db.Exec(`UPDATE flats SET visibility='private'
		WHERE visibility='public' AND live_version=0
		AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.flat=flats.slug AND v.published=1)`); err != nil {
		return err
	}
	// Rewrite pending visibility requests onto the two-value vocabulary.
	rows, err := db.Query(`SELECT id, params FROM approvals WHERE action='set_visibility' AND status IN ('pending','applying')`)
	if err != nil {
		return err
	}
	type row struct {
		id     string
		params string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.params); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range pending {
		var p map[string]any
		if err := json.Unmarshal([]byte(r.params), &p); err != nil {
			continue
		}
		changed := false
		for _, key := range []string{"visibility", "from", "to"} {
			s, _ := p[key].(string)
			if s == string(PublicListed) || s == string(PublicUnlisted) {
				p[key] = string(Public)
				changed = true
			}
		}
		if !changed {
			continue
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE approvals SET params=? WHERE id=?`, string(raw), r.id); err != nil {
			return err
		}
	}

	return nil
}

// UnpublishedVersions returns saved-but-never-deployed version rows, oldest first.
func (s *Store) UnpublishedVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+versionCols+` FROM versions WHERE published=0 ORDER BY flat, number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteVersion removes a version row. It does not touch files.
func (s *Store) DeleteVersion(ctx context.Context, flat string, n int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM versions WHERE flat=? AND number=?`, flat, n)
	return err
}

// CountPublished returns how many published versions a flat has.
func (s *Store) CountPublished(ctx context.Context, flat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE flat=? AND published=1`, flat).Scan(&n)
	return n, err
}

// PutDraftRevision inserts an immutable draft snapshot.
func (s *Store) PutDraftRevision(ctx context.Context, r DraftRevision) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO draft_revisions(flat,revision,hash,size,files,kind,manifest,git_sha,git_dirty,message,screenshot,created_at,pruned,legacy_number) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Flat, r.Revision, r.Hash, r.Size, r.Files, r.Kind, string(r.Manifest), r.GitSHA, r.GitDirty, r.Message, r.Screenshot, unix(r.CreatedAt), r.Pruned, r.LegacyNumber)
	return err
}

// PutDraft sets the current draft pointer.
func (s *Store) PutDraft(ctx context.Context, d Draft) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO drafts(flat,revision,hash,base_version,dirty,size,files,kind,manifest,git_sha,git_dirty,message,updated_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(flat) DO UPDATE SET revision=excluded.revision, hash=excluded.hash, base_version=excluded.base_version, dirty=excluded.dirty,
			size=excluded.size, files=excluded.files, kind=excluded.kind, manifest=excluded.manifest, git_sha=excluded.git_sha, git_dirty=excluded.git_dirty,
			message=excluded.message, updated_at=excluded.updated_at`,
		d.Flat, d.Revision, d.Hash, d.BaseVersion, d.Dirty, d.Size, d.Files, d.Kind, string(d.Manifest), d.GitSHA, d.GitDirty, d.Message, unix(d.UpdatedAt), unix(d.CreatedAt))
	return err
}

func scanDraft(sc interface{ Scan(...any) error }) (Draft, error) {
	var d Draft
	var manifest string
	var git, msg sql.NullString
	var updated, created int64
	var dirty, gitDirty int
	if err := sc.Scan(&d.Flat, &d.Revision, &d.Hash, &d.BaseVersion, &dirty, &d.Size, &d.Files, &d.Kind, &manifest, &git, &gitDirty, &msg, &updated, &created); err != nil {
		return d, err
	}
	d.Dirty = dirty == 1
	d.GitDirty = gitDirty == 1
	d.Manifest = json.RawMessage(manifest)
	d.GitSHA, d.Message = git.String, msg.String
	d.UpdatedAt, d.CreatedAt = fromUnix(updated), fromUnix(created)
	return d, nil
}

// GetDraft returns the current draft.
func (s *Store) GetDraft(ctx context.Context, flat string) (Draft, error) {
	d, err := scanDraft(s.db.QueryRowContext(ctx, `SELECT flat,revision,hash,base_version,CASE WHEN hash = (SELECT v.hash FROM versions v JOIN flats f ON f.slug=v.flat AND f.live_version=v.number WHERE f.slug=drafts.flat) THEN 0 ELSE 1 END,size,files,kind,manifest,git_sha,git_dirty,message,updated_at,created_at FROM drafts WHERE flat=?`, flat))
	if errors.Is(err, sql.ErrNoRows) {
		return d, fmt.Errorf("draft of %q: %w", flat, ErrNotFound)
	}
	return d, err
}

func scanDraftRevision(sc interface{ Scan(...any) error }) (DraftRevision, error) {
	var r DraftRevision
	var manifest string
	var git, msg, shot sql.NullString
	var created int64
	var pruned, gitDirty int
	if err := sc.Scan(&r.Flat, &r.Revision, &r.Hash, &r.Size, &r.Files, &r.Kind, &manifest, &git, &gitDirty, &msg, &shot, &created, &pruned, &r.LegacyNumber); err != nil {
		return r, err
	}
	r.Manifest = json.RawMessage(manifest)
	r.GitSHA, r.Message, r.Screenshot = git.String, msg.String, shot.String
	r.GitDirty = gitDirty == 1
	r.Pruned = pruned == 1
	r.CreatedAt = fromUnix(created)
	return r, nil
}

const draftRevCols = `flat,revision,hash,size,files,kind,manifest,git_sha,git_dirty,message,screenshot,created_at,pruned,legacy_number`

// GetDraftRevision returns one snapshot.
func (s *Store) GetDraftRevision(ctx context.Context, flat string, revision int) (DraftRevision, error) {
	r, err := scanDraftRevision(s.db.QueryRowContext(ctx, `SELECT `+draftRevCols+` FROM draft_revisions WHERE flat=? AND revision=?`, flat, revision))
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("draft revision %d of %q: %w", revision, flat, ErrNotFound)
	}
	return r, err
}

// NextDraftRevision returns the next revision number for flat.
func (s *Store) NextDraftRevision(ctx context.Context, flat string) (int, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(revision) FROM draft_revisions WHERE flat=?`, flat).Scan(&n); err != nil {
		return 0, err
	}
	return int(n.Int64) + 1, nil
}

// ListDraftRevisions returns snapshots newest first.
func (s *Store) ListDraftRevisions(ctx context.Context, flat string) ([]DraftRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+draftRevCols+` FROM draft_revisions WHERE flat=? ORDER BY revision DESC`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DraftRevision
	for rows.Next() {
		r, err := scanDraftRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDraftRevisionPruned flags a draft snapshot whose files were removed.
func (s *Store) MarkDraftRevisionPruned(ctx context.Context, flat string, revision int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE draft_revisions SET pruned=1 WHERE flat=? AND revision=?`, flat, revision)
	return err
}

// FindOpenApprovalByKey returns the pending or applying approval for key.
func (s *Store) FindOpenApprovalByKey(ctx context.Context, key string) (Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE idempotency_key=? AND status IN ('pending','applying')`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("approval key: %w", ErrNotFound)
	}
	return a, err
}

// DeploymentByApproval returns the deployment recorded for an approval, if any.
func (s *Store) DeploymentByApproval(ctx context.Context, approvalID string) (Deployment, bool, error) {
	var d Deployment
	var at int64
	var approval sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT version,previous,kind,at,approval_id FROM deployments WHERE approval_id=? ORDER BY id DESC LIMIT 1`, approvalID).
		Scan(&d.Version, &d.Previous, &d.Kind, &at, &approval)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.At = fromUnix(at)
	d.ApprovalID = approval.String
	return d, true, nil
}

// SetProviderPermission records whether flat may use provider.
func (s *Store) SetProviderPermission(ctx context.Context, flat, provider string, permitted bool) error {
	bit := 0
	if permitted {
		bit = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO flat_providers(flat, provider, permitted) VALUES(?,?,?)
		ON CONFLICT(flat, provider) DO UPDATE SET permitted=excluded.permitted`, flat, provider, bit)
	return err
}

// ProviderPermitted reports an explicit non-local permission. Local is always permitted.
func (s *Store) ProviderPermitted(ctx context.Context, flat, provider string) (bool, error) {
	if provider == ProviderLocal {
		return true, nil
	}
	var bit int
	err := s.db.QueryRowContext(ctx, `SELECT permitted FROM flat_providers WHERE flat=? AND provider=?`, flat, provider).Scan(&bit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bit == 1, nil
}

// PermittedProviders returns provider ids the flat may use, including local, sorted by the caller.
func (s *Store) PermittedProviders(ctx context.Context, flat string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider FROM flat_providers WHERE flat=? AND permitted=1 ORDER BY provider`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{ProviderLocal}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if p != ProviderLocal {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// CommitPublished atomically inserts a successful publication and its live history.
func (s *Store) CommitPublished(ctx context.Context, v Version, previous int, approval string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO versions(flat,number,hash,size,files,kind,manifest,git_sha,git_dirty,message,created_at,screenshot,published) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1)`, v.Flat, v.Number, v.Hash, v.Size, v.Files, v.Kind, string(v.Manifest), v.GitSHA, v.GitDirty, v.Message, unix(v.CreatedAt), v.Screenshot); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE flats SET live_version=?,updated_at=? WHERE slug=? AND live_version=?`, v.Number, unix(now), v.Flat, previous)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("live version changed concurrently")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployments(flat,version,previous,kind,at,approval_id) VALUES(?,?,?,'publish',?,?)`, v.Flat, v.Number, previous, unix(now), approval); err != nil {
		return err
	}
	return tx.Commit()
}

// AdoptLegacyDraft preserves references and switches database pointers atomically.
func (s *Store) AdoptLegacyDraft(ctx context.Context, r DraftRevision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE previews SET target='draft',revision=?,version=0 WHERE flat=? AND version=? AND target='version'`, r.Revision, r.Flat, r.LegacyNumber); err != nil {
		return err
	}
	// Old deploy/rollback numeric references must not alias a reused publication number.
	rows, err := tx.QueryContext(ctx, `SELECT id,params FROM approvals WHERE flat=? AND status IN ('pending','applying') AND action IN ('deploy','activate','rollback','publish')`, r.Flat)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var p struct {
			Version int `json:"version"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		if p.Version == r.LegacyNumber {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status='failed',result='legacy version migrated to draft; request a new publish approval' WHERE id=?`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO drafts(flat,revision,hash,base_version,dirty,size,files,kind,manifest,git_sha,git_dirty,message,updated_at,created_at) SELECT ?,?,?,live_version,1,?,?,?,?,?,?,?, ?,? FROM flats WHERE slug=? ON CONFLICT(flat) DO UPDATE SET revision=excluded.revision,hash=excluded.hash,dirty=1,size=excluded.size,files=excluded.files,kind=excluded.kind,manifest=excluded.manifest,git_sha=excluded.git_sha,git_dirty=excluded.git_dirty,message=excluded.message,updated_at=excluded.updated_at WHERE drafts.revision<excluded.revision`, r.Flat, r.Revision, r.Hash, r.Size, r.Files, r.Kind, string(r.Manifest), r.GitSHA, r.GitDirty, r.Message, unix(r.CreatedAt), unix(r.CreatedAt), r.Flat); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM versions WHERE flat=? AND number=? AND published=0`, r.Flat, r.LegacyNumber); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishApprovalData durably records the typed execution result.
func (s *Store) FinishApprovalData(ctx context.Context, id, status, result string, data json.RawMessage, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status=?,result=?,result_data=?,decided_at=? WHERE id=? AND status='applying'`, status, result, string(data), unix(now), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("approval no longer applying")
	}
	return nil
}

// ClaimApprovalAuthorized records validated operator authority before execution.
func (s *Store) ClaimApprovalAuthorized(ctx context.Context, id, actor string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='applying',decided_by=?,authorized_at=? WHERE id=? AND status='pending'`, actor, unix(now), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("approval no longer pending")
	}
	return nil
}

// RejectApprovalAuthorized atomically finishes a pending rejection with its
// validated operator audit. It cannot overwrite a claimed or decided approval.
func (s *Store) RejectApprovalAuthorized(ctx context.Context, id, actor, result string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='rejected',result=?,decided_by=?,authorized_at=?,decided_at=? WHERE id=? AND status='pending'`, result, actor, unix(now), unix(now), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("approval no longer pending")
	}
	return nil
}

// CommitVisibility records policy and an execution receipt in one transaction.
func (s *Store) CommitVisibility(ctx context.Context, f Flat, from Visibility, approval string, data json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE flats SET visibility=?,updated_at=? WHERE slug=? AND visibility=? AND live_version=?`, string(f.Visibility), unix(f.UpdatedAt), f.Slug, string(from), f.LiveVersion)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("visibility changed concurrently")
	}
	res, err = tx.ExecContext(ctx, `UPDATE approvals SET result_data=? WHERE id=? AND status='applying'`, string(data), approval)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return errors.New("approval no longer applying")
	}
	return tx.Commit()
}

// PreserveLegacyTailscale is called only for an explicit operator selection of
// --network tailscale. Existing explicit allow/deny rows win; newly-created
// flats are not eligible, and this never grants Funnel or Portal. Consuming
// eligibility with the insert prevents a restart from undoing later revocation.
func (s *Store) PreserveLegacyTailscale(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO flat_providers(flat,provider,permitted) SELECT flat,'tailscale',1 FROM legacy_private_upgrade WHERE pending=1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE legacy_private_upgrade SET pending=0 WHERE pending=1`); err != nil {
		return err
	}
	return tx.Commit()
}

// LegacyPrivateUpgradePending reports unconsumed pre-lifecycle identities.
func (s *Store) LegacyPrivateUpgradePending(ctx context.Context) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_private_upgrade WHERE pending=1)`).Scan(&pending)
	return pending, err
}

// DeclineLegacyTailscale records an explicit Local-only upgrade choice without
// inventing permissions. Future default starts need not ask again.
func (s *Store) DeclineLegacyTailscale(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE legacy_private_upgrade SET pending=0 WHERE pending=1`)
	return err
}
