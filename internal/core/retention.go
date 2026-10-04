package core

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RetentionImpact is what the next pruning removes under a candidate
// retention setting: versions for keep_versions, events for events_keep
// and previews for preview_ttl_seconds.
type RetentionImpact struct {
	Current   string `json:"current"`
	Candidate string `json:"candidate"`
	// Total counts everything removed, over every flat.
	Total int64 `json:"total"`
	// Flats lists up to impactFlats flats, most affected first.
	Flats []FlatImpact `json:"flats"`
	// More counts the affected flats not listed.
	More int `json:"more,omitempty"`
}

// FlatImpact is what pruning removes from one flat.
type FlatImpact struct {
	Slug  string `json:"slug"`
	Count int64  `json:"count"`
	// Versions lists up to impactItems version numbers, newest first.
	Versions []int `json:"versions,omitempty"`
	// Previews lists up to impactItems preview hosts.
	Previews []string `json:"previews,omitempty"`
}

const (
	impactFlats = 10
	impactItems = 10
)

// RetentionImpact reports, for each decrease of keep_versions, events_keep
// or preview_ttl_seconds in candidate, what the next pruning would remove
// with the candidate value: the version files pruned after a flat's next
// deploy that keeps its live version live (the deploy closes the flat's
// previews first), the log events trimmed by the next sweep and the
// previews it closes. It changes nothing. Other settings in candidate are validated
// but have no impact.
func (s *Service) RetentionImpact(ctx context.Context, candidate map[string]string) (map[string]RetentionImpact, error) {
	clean, err := cleanSettings(candidate)
	if err != nil {
		return nil, err
	}
	current := s.settings.all()
	out := map[string]RetentionImpact{}
	for _, k := range []string{SetKeepVersions, SetEventsKeep, SetPreviewTTL} {
		v, ok := clean[k]
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		cur, _ := strconv.ParseInt(current[k], 10, 64)
		if !retentionDecrease(k, cur, n) {
			continue
		}
		var flats []FlatImpact
		switch k {
		case SetKeepVersions:
			flats, err = s.versionImpact(ctx, int(n))
		case SetEventsKeep:
			flats, err = s.eventImpact(ctx, int(n))
		case SetPreviewTTL:
			flats = s.previewImpact(durationOf(n, time.Second))
		}
		if err != nil {
			return nil, err
		}
		out[k] = summarize(current[k], v, flats)
	}
	return out, nil
}

// retentionDecrease reports whether n keeps less than cur. keep_versions 0
// keeps every version.
func retentionDecrease(k string, cur, n int64) bool {
	if k == SetKeepVersions {
		return n != 0 && (cur == 0 || n < cur)
	}
	return n < cur
}

func (s *Service) versionImpact(ctx context.Context, keep int) ([]FlatImpact, error) {
	fs, err := s.st.ListFlats(ctx)
	if err != nil {
		return nil, err
	}
	var out []FlatImpact
	for _, f := range fs {
		// Pruning runs only at the end of a deploy, which closes the
		// flat's previews first: they protect no version then.
		prune, err := s.prunableVersions(ctx, f.Slug, keep, false)
		if err != nil {
			return nil, err
		}
		if len(prune) > 0 {
			out = append(out, FlatImpact{Slug: f.Slug, Count: int64(len(prune)), Versions: prune[:min(len(prune), impactItems)]})
		}
	}
	return out, nil
}

func (s *Service) eventImpact(ctx context.Context, keep int) ([]FlatImpact, error) {
	flats, err := s.st.EventFlats(ctx)
	if err != nil {
		return nil, err
	}
	var out []FlatImpact
	for _, f := range flats {
		n, err := s.st.PrunableEvents(ctx, f, keep)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, FlatImpact{Slug: f, Count: n})
		}
	}
	return out, nil
}

func (s *Service) previewImpact(ttl time.Duration) []FlatImpact {
	expired := s.expiredPreviews(ttl, s.now())
	slices.SortFunc(expired, func(a, b *preview) int { return strings.Compare(a.host, b.host) })
	index := map[string]int{}
	var out []FlatImpact
	for _, p := range expired {
		i, ok := index[p.flat]
		if !ok {
			i = len(out)
			index[p.flat] = i
			out = append(out, FlatImpact{Slug: p.flat})
		}
		out[i].Count++
		if len(out[i].Previews) < impactItems {
			out[i].Previews = append(out[i].Previews, p.host)
		}
	}
	return out
}

// summarize totals flats and keeps the most affected ones.
func summarize(current, candidate string, flats []FlatImpact) RetentionImpact {
	r := RetentionImpact{Current: current, Candidate: candidate, Flats: []FlatImpact{}}
	slices.SortStableFunc(flats, func(a, b FlatImpact) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Slug, b.Slug))
	})
	for _, f := range flats {
		r.Total += f.Count
	}
	if len(flats) > impactFlats {
		r.More = len(flats) - impactFlats
		flats = flats[:impactFlats]
	}
	r.Flats = append(r.Flats, flats...)
	return r
}
