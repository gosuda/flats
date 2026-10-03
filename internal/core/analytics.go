package core

import (
	"context"

	"github.com/oesni/flats/internal/store"
)

type trafficKey struct {
	day  string
	path string
}

// recordPath bounds the buffer even when a visitor requests many distinct URLs.
// Query strings never enter analytics; they may contain private information.
func (lf *liveFlat) recordPath(day, path string) {
	lf.trafficMu.Lock()
	defer lf.trafficMu.Unlock()
	if lf.traffic == nil {
		lf.traffic = map[trafficKey]int64{}
	}
	if len(path) > 1024 {
		path = "(other)"
	}
	key := trafficKey{day: day, path: path}
	if _, exists := lf.traffic[key]; !exists && len(lf.traffic) >= 1000 {
		key.path = "(other)"
	}
	lf.traffic[key]++
}

func (s *Service) flushPaths(ctx context.Context, slug string, lf *liveFlat) {
	lf.trafficMu.Lock()
	defer lf.trafficMu.Unlock()
	if len(lf.traffic) == 0 {
		return
	}
	rows := make([]store.PathCount, 0, len(lf.traffic))
	for key, count := range lf.traffic {
		rows = append(rows, store.PathCount{Day: key.day, Path: key.path, Count: count})
	}
	if err := s.st.AddPathCounts(ctx, slug, rows); err == nil {
		lf.traffic = map[trafficKey]int64{}
	}
}

// TopPages returns per-path page request counts within a UTC calendar period.
func (s *Service) TopPages(ctx context.Context, slug string, days int) ([]store.PathCount, error) {
	if _, err := s.st.GetFlat(ctx, slug); err != nil {
		return nil, err
	}
	s.mu.Lock()
	lf := s.live[slug]
	s.mu.Unlock()
	if lf != nil {
		s.flushPaths(ctx, slug, lf)
	}
	since := s.now().UTC().AddDate(0, 0, 1-days).Format("2006-01-02")
	return s.st.TopPages(ctx, slug, since)
}
