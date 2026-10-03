package store

import "context"

// PathCount is a path's request count; Day is omitted from period totals.
type PathCount struct {
	Day   string `json:"day,omitempty"`
	Path  string `json:"path"`
	Count int64  `json:"count"`
}

// AddPathCounts persists one buffered batch atomically, so retries do not
// duplicate a partially written batch.
func (s *Store) AddPathCounts(ctx context.Context, flat string, counts []PathCount) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range counts {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO pagepaths(flat,day,path,count) VALUES(?,?,?,?)
			 ON CONFLICT(flat,day,path) DO UPDATE SET count=count+excluded.count`,
			flat, row.Day, row.Path, row.Count,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TopPages returns the 100 most requested paths since the supplied UTC day.
func (s *Store) TopPages(ctx context.Context, flat, since string) ([]PathCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path,SUM(count) FROM pagepaths WHERE flat=? AND day>=?
		 GROUP BY path ORDER BY SUM(count) DESC,path LIMIT 100`,
		flat, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PathCount{}
	for rows.Next() {
		var row PathCount
		if err := rows.Scan(&row.Path, &row.Count); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// PageViewsSince filters by calendar date rather than taking the last N days
// with traffic, which would include old visits in sparse analytics periods.
func (s *Store) PageViewsSince(ctx context.Context, flat, since string) ([]DayCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day,count FROM pageviews WHERE flat=? AND day>=? ORDER BY day`,
		flat, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayCount{}
	for rows.Next() {
		var row DayCount
		if err := rows.Scan(&row.Day, &row.Count); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
