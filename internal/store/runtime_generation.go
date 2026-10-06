package store

import "context"

// NextRuntimeGeneration durably allocates a host-wide ordering token. It is
// independent of published version numbers and survives host/worker restarts.
func (s *Store) NextRuntimeGeneration(ctx context.Context) (int64, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO runtime_generation(id,generation) SELECT 1,coalesce(max(number),0)+1 FROM versions WHERE true
ON CONFLICT(id) DO UPDATE SET generation=generation+1
WHERE generation < 9007199254740991 RETURNING generation`).Scan(&generation)
	return generation, err
}
