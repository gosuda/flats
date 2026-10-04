package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/portal-tunnel/v2/utils"

	"github.com/gosuda/flats/internal/config"
)

// Setting keys (all configurable in system settings). Each is stored in
// config.json under the key in settingKeys.
const (
	SetUploadMaxBytes = "upload_max_bytes"
	SetKeepVersions   = "keep_versions"
	SetDiskQuotaBytes = "disk_quota_bytes"
	SetPreviewTTL     = "preview_ttl_seconds"
	SetRateLimit      = "rate_limit_rps"
	SetPortalRelays   = "portal_relays" // comma-separated; empty = Portal defaults
	SetRedirectDays   = "redirect_days"
	SetPortalDiscover = "portal_discovery"  // "true" (Portal CLI default) or "false"
	SetPortalMaxRelay = "portal_max_relays" // active relays chosen by discovery (default 3)
	SetEventsKeep     = "events_keep"       // log events kept per flat
)

// settingKeys maps each setting to its config.json key.
var settingKeys = map[string]string{
	SetUploadMaxBytes: "system.upload_max_bytes",
	SetKeepVersions:   "system.keep_versions",
	SetDiskQuotaBytes: "system.disk_quota_bytes",
	SetPreviewTTL:     "system.preview_ttl_seconds",
	SetRateLimit:      "system.rate_limit_rps",
	SetRedirectDays:   "system.redirect_days",
	SetEventsKeep:     "system.events_keep",
	SetPortalRelays:   "portal.relays",
	SetPortalDiscover: "portal.discovery",
	SetPortalMaxRelay: "portal.max_active_relays",
}

// SettingConfigKey returns the config.json key of a setting.
func SettingConfigKey(setting string) (string, bool) {
	k, ok := settingKeys[setting]
	return k, ok
}

// Defaults are the factory settings: the frozen config.json defaults.
var Defaults = func() map[string]string {
	out := make(map[string]string, len(settingKeys))
	for k, ck := range settingKeys {
		v, ok := config.Default(ck)
		if !ok {
			panic("core: no config key " + ck)
		}
		out[k] = settingString(v)
	}
	return out
}()

// settingString formats a config value the way the settings API shows it.
func settingString(v any) string {
	switch v := v.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	case []string:
		return strings.Join(v, ",")
	case string:
		return v
	}
	return fmt.Sprint(v)
}

// SettingsSource holds the system settings as a config.json document. Save,
// when set, persists a change before it is applied; nil keeps settings in
// memory only, as for a Service built without one.
type SettingsSource struct {
	mu     sync.Mutex
	doc    *config.Document
	values map[string]string
	save   func(ctx context.Context, apply func(*config.Document) error) error
}

// NewSettingsSource serves the settings of doc. save receives the change as
// a function to apply to the document it writes.
func NewSettingsSource(doc *config.Document, save func(ctx context.Context, apply func(*config.Document) error) error) (*SettingsSource, error) {
	src := &SettingsSource{doc: doc.Clone(), save: save}
	if err := src.refresh(); err != nil {
		return nil, err
	}
	return src, nil
}

func memorySettings() *SettingsSource {
	doc, err := config.New(config.NewInstanceID(), string(filepath.Separator))
	if err == nil {
		var src *SettingsSource
		if src, err = NewSettingsSource(doc, nil); err == nil {
			return src
		}
	}
	panic(err) // the default document is always valid
}

// refresh recomputes the settings view of s.doc.
func (s *SettingsSource) refresh() error {
	c, err := s.doc.Effective(nil)
	if err != nil {
		return err
	}
	values := make(map[string]string, len(settingKeys))
	for k, ck := range settingKeys {
		v, _, _ := c.Lookup(ck)
		values[k] = settingString(v)
	}
	s.values = values
	return nil
}

func (s *SettingsSource) get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

func (s *SettingsSource) all() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.values)
}

// update validates values against the config rules, persists them and only
// then makes them current. Nothing changes unless every value is accepted.
func (s *SettingsSource) update(ctx context.Context, values map[string]string) error {
	apply := func(d *config.Document) error {
		for _, k := range slices.Sorted(maps.Keys(values)) {
			if err := d.Set(settingKeys[k], values[k]); err != nil {
				return err
			}
		}
		return d.Validate()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.doc.Clone()
	if err := apply(next); err != nil {
		return invalid(err)
	}
	if s.save != nil {
		if err := s.save(ctx, apply); err != nil {
			if errors.Is(err, config.ErrConflict) {
				return withKind(ErrConflict, fmt.Errorf("config.json changed on disk since Flats loaded it; restart `flats serve` to load the file, then retry: %w", err))
			}
			var fe *config.FieldError
			if errors.As(err, &fe) {
				return invalid(err)
			}
			return err
		}
	}
	prev := s.doc
	s.doc = next
	if err := s.refresh(); err != nil {
		s.doc = prev
		return err
	}
	return nil
}

func (s *Service) setting(key string) string { return s.settings.get(key) }

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
	return durationOf(s.intSetting(SetPreviewTTL), time.Second)
}
func (s *Service) rateLimit() float64 { return float64(s.intSetting(SetRateLimit)) }
func (s *Service) redirectWindow() time.Duration {
	return durationOf(s.intSetting(SetRedirectDays), 24*time.Hour)
}

// durationOf is n units, saturated at the longest duration: config.json
// allows counts whose product would overflow.
func durationOf(n int64, unit time.Duration) time.Duration {
	if n > int64(math.MaxInt64/unit) {
		return math.MaxInt64
	}
	return time.Duration(n) * unit
}

// Settings returns every setting with defaults filled in.
func (s *Service) Settings(ctx context.Context) (map[string]string, error) {
	return s.settings.all(), nil
}

// UpdateSettings validates and stores settings. Only the console calls it.
// Nothing is stored unless every value is valid. The Portal settings are
// read when `flats serve` starts.
func (s *Service) UpdateSettings(ctx context.Context, in map[string]string) (map[string]string, error) {
	clean := make(map[string]string, len(in))
	for k, v := range in {
		if _, ok := Defaults[k]; !ok {
			return nil, invalidf("unknown setting %q", k)
		}
		norm, err := normalizeSetting(k, strings.TrimSpace(v))
		if err != nil {
			return nil, invalid(err)
		}
		clean[k] = norm
	}
	if err := s.settings.update(ctx, clean); err != nil {
		return nil, err
	}
	if _, ok := clean[SetRateLimit]; ok {
		s.mu.Lock()
		for _, lf := range s.live {
			lf.limiter.setRate(s.rateLimit())
		}
		s.mu.Unlock()
	}
	return s.Settings(ctx)
}

// normalizeSetting validates one setting value and returns the form to store.
func normalizeSetting(k, v string) (string, error) {
	switch k {
	case SetPortalRelays:
		var relays []string
		for part := range strings.SplitSeq(v, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			u, err := utils.NormalizeRelayURL(part)
			if err != nil {
				return "", fmt.Errorf("setting %s: relay %q: %v (use https://host[:port], or leave it empty for Portal defaults)", k, part, err)
			}
			if !slices.Contains(relays, u) {
				relays = append(relays, u)
			}
		}
		return strings.Join(relays, ","), nil
	case SetPortalDiscover:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return "", fmt.Errorf("setting %s must be true or false", k)
		}
		return strconv.FormatBool(b), nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return "", fmt.Errorf("setting %s must be a non-negative integer", k)
	}
	switch k {
	case SetUploadMaxBytes, SetPreviewTTL, SetRateLimit, SetPortalMaxRelay, SetEventsKeep:
		if n == 0 {
			return "", fmt.Errorf("setting %s must be positive", k)
		}
	}
	return strconv.FormatInt(n, 10), nil
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
