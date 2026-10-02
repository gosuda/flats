package core

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Setting keys and defaults (all configurable in system settings).
const (
	SetUploadMaxBytes = "upload_max_bytes"
	SetKeepVersions   = "keep_versions"
	SetDiskQuotaBytes = "disk_quota_bytes"
	SetPreviewTTL     = "preview_ttl_seconds"
	SetRateLimit      = "rate_limit_rps"
	SetPortalRelays   = "portal_relays" // comma-separated; empty = Portal defaults
	SetRedirectDays   = "redirect_days"
)

// Defaults are the factory settings.
var Defaults = map[string]string{
	SetUploadMaxBytes: strconv.Itoa(20 << 20),
	SetKeepVersions:   "10",
	SetDiskQuotaBytes: strconv.FormatInt(30<<30, 10),
	SetPreviewTTL:     strconv.Itoa(24 * 3600),
	SetRateLimit:      "50",
	SetPortalRelays:   "",
	SetRedirectDays:   "7",
}


func (s *Service) setting(key string) string {
	if v, ok := s.settings.Load(key); ok {
		return v.(string)
	}
	v, err := s.st.GetSetting(context.Background(), key, Defaults[key])
	if err != nil {
		v = Defaults[key]
	}
	s.settings.Store(key, v)
	return v
}

func (s *Service) intSetting(key string) int64 {
	n, err := strconv.ParseInt(s.setting(key), 10, 64)
	if err != nil {
		n, _ = strconv.ParseInt(Defaults[key], 10, 64)
	}
	return n
}

// UploadLimit is the maximum uncompressed upload size in bytes.
func (s *Service) UploadLimit() int64 { return s.intSetting(SetUploadMaxBytes) }
func (s *Service) keepVersions() int  { return int(s.intSetting(SetKeepVersions)) }
func (s *Service) diskQuota() int64   { return s.intSetting(SetDiskQuotaBytes) }
func (s *Service) previewTTL() time.Duration {
	return time.Duration(s.intSetting(SetPreviewTTL)) * time.Second
}
func (s *Service) rateLimit() float64 { return float64(s.intSetting(SetRateLimit)) }
func (s *Service) redirectWindow() time.Duration {
	return time.Duration(s.intSetting(SetRedirectDays)) * 24 * time.Hour
}

// Settings returns every setting with defaults filled in.
func (s *Service) Settings(ctx context.Context) (map[string]string, error) {
	stored, err := s.st.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range Defaults {
		out[k] = v
	}
	for k, v := range stored {
		if _, known := Defaults[k]; known {
			out[k] = v
		}
	}
	return out, nil
}

// UpdateSettings validates and stores settings. Only the console calls it.
func (s *Service) UpdateSettings(ctx context.Context, in map[string]string) (map[string]string, error) {
	for k, v := range in {
		if _, ok := Defaults[k]; !ok {
			return nil, fmt.Errorf("unknown setting %q", k)
		}
		if k == SetPortalRelays {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("setting %s must be a non-negative integer", k)
		}
		if (k == SetUploadMaxBytes || k == SetPreviewTTL || k == SetRateLimit) && n == 0 {
			return nil, fmt.Errorf("setting %s must be positive", k)
		}
	}
	for k, v := range in {
		if err := s.st.SetSetting(ctx, k, v); err != nil {
			return nil, err
		}
		s.settings.Store(k, v)
	}
	if _, ok := in[SetRateLimit]; ok {
		s.mu.Lock()
		for _, lf := range s.live {
			lf.limiter.setRate(s.rateLimit())
		}
		s.mu.Unlock()
	}
	return s.Settings(ctx)
}

// limiter is a token bucket (rate per second, burst 2x rate).
type limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newLimiter(rate float64) *limiter {
	return &limiter{rate: rate, tokens: rate * 2, last: time.Now()}
}

func (l *limiter) setRate(r float64) {
	l.mu.Lock()
	l.rate = r
	l.mu.Unlock()
}

func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rate <= 0 {
		return true
	}
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if max := l.rate * 2; l.tokens > max {
		l.tokens = max
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
