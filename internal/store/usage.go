package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
)

// UsageDay is one flat's request and worker usage on one UTC day
// (docs/internal/metrics.md, Storage).
type UsageDay struct {
	Day         string
	C1xx        int64
	C2xx        int64
	C3xx        int64
	C4xx        int64
	C5xx        int64
	Closed      int64
	RateLimited int64
	// Timed is the number of requests in Latency; DurationMS their summed
	// response time.
	Timed      int64
	DurationMS int64
	// LatencyVersion names the bucket bounds of Latency (0: no buckets).
	// Counts of different versions are never added together.
	LatencyVersion int
	Latency        []int64
	CPUMS          int64
	MemoryPeak     int64
}

// Requests is the number of requests of every class, closed included.
func (d UsageDay) Requests() int64 {
	return d.C1xx + d.C2xx + d.C3xx + d.C4xx + d.C5xx + d.Closed
}

type latencyJSON struct {
	V int     `json:"v"`
	B []int64 `json:"b"`
}

func encodeLatency(version int, b []int64) string {
	if version == 0 || len(b) == 0 {
		return ""
	}
	raw, _ := json.Marshal(latencyJSON{V: version, B: b})
	return string(raw)
}

func decodeLatency(s string) (int, []int64) {
	var l latencyJSON
	if s == "" || json.Unmarshal([]byte(s), &l) != nil || l.V <= 0 {
		return 0, nil
	}
	return l.V, l.B
}

// Add merges d into u: counts are added and the memory peak is the larger
// one. Latency of one version and length is added; between versions the newer
// one wins, replacing the older buckets, Timed and DurationMS of that day, so
// the rest of a day after an upgrade is never dropped because the stored row
// is older. Timed requests whose buckets cannot be merged are dropped with
// them, so Timed always counts the requests in Latency.
func (u *UsageDay) Add(d UsageDay) {
	u.C1xx += d.C1xx
	u.C2xx += d.C2xx
	u.C3xx += d.C3xx
	u.C4xx += d.C4xx
	u.C5xx += d.C5xx
	u.Closed += d.Closed
	u.RateLimited += d.RateLimited
	u.CPUMS += d.CPUMS
	u.MemoryPeak = max(u.MemoryPeak, d.MemoryPeak)
	switch {
	case d.LatencyVersion == 0 || d.Timed == 0:
	case u.LatencyVersion == 0 || u.Timed == 0 || d.LatencyVersion > u.LatencyVersion:
		u.LatencyVersion, u.Latency = d.LatencyVersion, append([]int64(nil), d.Latency...)
		u.Timed, u.DurationMS = d.Timed, d.DurationMS
	case u.LatencyVersion == d.LatencyVersion && len(u.Latency) == len(d.Latency):
		for i := range u.Latency {
			u.Latency[i] += d.Latency[i]
		}
		u.Timed += d.Timed
		u.DurationMS += d.DurationMS
	}
}

const usageCols = `day,c1xx,c2xx,c3xx,c4xx,c5xx,closed,rate_limited,timed,duration_ms,latency,cpu_ms,memory_peak`

func scanUsage(sc interface{ Scan(...any) error }) (UsageDay, error) {
	var d UsageDay
	var lat string
	if err := sc.Scan(&d.Day, &d.C1xx, &d.C2xx, &d.C3xx, &d.C4xx, &d.C5xx, &d.Closed, &d.RateLimited, &d.Timed, &d.DurationMS, &lat, &d.CPUMS, &d.MemoryPeak); err != nil {
		return d, err
	}
	d.LatencyVersion, d.Latency = decodeLatency(lat)
	if d.LatencyVersion == 0 {
		d.Timed, d.DurationMS = 0, 0
	}
	return d, nil
}

// AddUsage adds days to the usage of the flat whose ID is uid, in one
// transaction. It writes nothing and reports false when no flat has that ID
// any more: usage of a deleted flat must never attach to a new flat that
// reuses its slug.
func (s *Store) AddUsage(ctx context.Context, uid string, days []UsageDay) (bool, error) {
	if uid == "" {
		return false, nil
	}
	gone, err := s.AddUsageBatch(ctx, []UsageBatch{{UID: uid, Days: days}})
	return err == nil && len(gone) == 0, err
}

// UsageBatch is the usage of one flat for AddUsageBatch.
type UsageBatch struct {
	UID  string
	Days []UsageDay
}

// AddUsageBatch adds the usage of several flats in one transaction, as
// AddUsage does for each: the days of an ID that no flat has any more are
// discarded, and those IDs are returned. On an error nothing is written.
func (s *Store) AddUsageBatch(ctx context.Context, batches []UsageBatch) (gone []string, err error) {
	if len(batches) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, b := range batches {
		ok, err := addUsageTx(ctx, tx, b.UID, b.Days)
		if err != nil {
			return nil, err
		}
		if !ok {
			gone = append(gone, b.UID)
		}
	}
	return gone, tx.Commit()
}

// addUsageTx adds days to the flat with ID uid inside tx, or reports false
// when no flat has that ID.
func addUsageTx(ctx context.Context, tx *sql.Tx, uid string, days []UsageDay) (bool, error) {
	if uid == "" {
		return false, nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flats WHERE uid=?)`, uid).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	for _, d := range days {
		cur, err := scanUsage(tx.QueryRowContext(ctx, `SELECT `+usageCols+` FROM usagedays WHERE flat_uid=? AND day=?`, uid, d.Day))
		if errors.Is(err, sql.ErrNoRows) {
			cur = UsageDay{Day: d.Day}
		} else if err != nil {
			return false, err
		}
		cur.Add(d)
		if _, err := tx.ExecContext(ctx, `INSERT INTO usagedays(flat_uid,requests,`+usageCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(flat_uid,day) DO UPDATE SET requests=excluded.requests,c1xx=excluded.c1xx,c2xx=excluded.c2xx,c3xx=excluded.c3xx,
			c4xx=excluded.c4xx,c5xx=excluded.c5xx,closed=excluded.closed,rate_limited=excluded.rate_limited,timed=excluded.timed,
			duration_ms=excluded.duration_ms,latency=excluded.latency,cpu_ms=excluded.cpu_ms,memory_peak=excluded.memory_peak`,
			uid, cur.Requests(), cur.Day, cur.C1xx, cur.C2xx, cur.C3xx, cur.C4xx, cur.C5xx, cur.Closed, cur.RateLimited,
			cur.Timed, cur.DurationMS, encodeLatency(cur.LatencyVersion, cur.Latency), cur.CPUMS, cur.MemoryPeak); err != nil {
			return false, err
		}
	}
	return true, nil
}

// UsageSince returns the usage of the flat with ID uid from the UTC day since
// on, oldest first. Days without usage are absent.
func (s *Store) UsageSince(ctx context.Context, uid, since string) ([]UsageDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+usageCols+` FROM usagedays WHERE flat_uid=? AND day>=? ORDER BY day`, uid, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageDay{}
	for rows.Next() {
		d, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FlatType is what host metrics need of one flat, read in one query.
type FlatType struct {
	Slug       string
	UID        string
	Visibility Visibility
	// Manifest and Kind are those of the live version, else of the newest
	// version, else of the Draft (as the flat view takes its type); nil and
	// "" when the flat has none of them.
	Manifest json.RawMessage
	Kind     string
}

const flatTypeQuery = `SELECT f.slug, COALESCE(f.uid,''), f.visibility,
  COALESCE(lv.manifest, nv.manifest, d.manifest), COALESCE(lv.kind, nv.kind, d.kind, '')
FROM flats f
LEFT JOIN versions lv ON f.live_version>0 AND lv.flat=f.slug AND lv.number=f.live_version AND lv.published=1
LEFT JOIN versions nv ON nv.flat=f.slug AND nv.published=1 AND nv.number=(SELECT MAX(number) FROM versions WHERE flat=f.slug AND published=1)
LEFT JOIN drafts d ON d.flat=f.slug`

// FlatTypes returns every flat's slug, ID, visibility and type source in one
// read, ordered by slug.
func (s *Store) FlatTypes(ctx context.Context) ([]FlatType, error) {
	rows, err := s.db.QueryContext(ctx, flatTypeQuery+` ORDER BY f.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlatType
	for rows.Next() {
		ft, err := scanFlatType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ft)
	}
	return out, rows.Err()
}

// FlatTypeOf is FlatTypes for one flat.
func (s *Store) FlatTypeOf(ctx context.Context, slug string) (FlatType, error) {
	ft, err := scanFlatType(s.db.QueryRowContext(ctx, flatTypeQuery+` WHERE f.slug=?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return ft, ErrNotFound
	}
	return ft, err
}

func scanFlatType(sc interface{ Scan(...any) error }) (FlatType, error) {
	var ft FlatType
	var vis string
	var manifest sql.NullString
	if err := sc.Scan(&ft.Slug, &ft.UID, &vis, &manifest, &ft.Kind); err != nil {
		return ft, err
	}
	ft.Visibility = Visibility(vis)
	if manifest.Valid {
		ft.Manifest = json.RawMessage(manifest.String)
	}
	return ft, nil
}

// PendingApprovals counts the approvals waiting for the operator.
func (s *Store) PendingApprovals(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE status='pending'`).Scan(&n)
	return n, err
}

// Size is the size in bytes of the database file and its write-ahead log.
// It reads file sizes only and never queries the database.
func (s *Store) Size() (int64, error) {
	if s.path == "" {
		return 0, errors.New("store has no file")
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return 0, err
	}
	n := fi.Size()
	// The log may legitimately be absent (fully checkpointed).
	if fi, err := os.Stat(s.path + "-wal"); err == nil {
		n += fi.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return n, nil
}
