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

// SettingsSource holds the system settings as a config.json document. With
// a file, a change is saved to it before it is applied; without one,
// settings stay in memory, as for a Service built without a source.
type SettingsSource struct {
	mu     sync.Mutex
	doc    *config.Document
	values map[string]string
	pinned map[string]string // setting -> who sets it
	file   *SettingsFile
}

// SettingsFile is the config.json behind a SettingsSource.
type SettingsFile struct {
	// Mode is "config" for a host started with --config or FLATS_CONFIG and
	// "legacy" for one started with the legacy flags.
	Mode string
	// Hash is the hash of the file bytes the host last read or wrote, the
	// settings ETag. Saves advance it.
	Hash string
	// Overrides are this run's host.* values from flags; never stored.
	Overrides map[string]string
	// Save applies apply to the file and returns the new hash, refusing
	// with a config.ErrConflict error if the file's hash is not expected.
	Save func(expected string, apply func(*config.Document) error) (string, error)
	// DiskHash returns the hash of the file on disk now.
	DiskHash func() (string, error)
}

// ErrConfigOverridden rejects a change to a setting that the running
// process takes from elsewhere, such as a service flag.
var ErrConfigOverridden = fmt.Errorf("%w: setting overridden", ErrConflict)

// ErrConfigChanged rejects a conditional settings change whose ETag is not
// the one of the settings in effect: config.json changed on disk, or another
// change was saved after the caller read the settings.
var ErrConfigChanged = errors.New("config changed")

// Pin refuses changes to setting: by names what sets it, such as "the
// service's --relays flag". Saving another value would make the next start
// disagree with that source.
func (s *SettingsSource) Pin(setting, by string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned == nil {
		s.pinned = map[string]string{}
	}
	s.pinned[setting] = by
}

// NewSettingsSource serves the settings of doc, saved to file (nil keeps
// them in memory).
func NewSettingsSource(doc *config.Document, file *SettingsFile) (*SettingsSource, error) {
	src := &SettingsSource{doc: doc.Clone(), file: file}
	if file != nil {
		f := *file
		f.Overrides = maps.Clone(file.Overrides)
		src.file = &f
	}
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

// etag returns the current settings ETag ("" without a file).
func (s *SettingsSource) etag() string {
	if s.file == nil {
		return ""
	}
	return s.file.Hash
}

// update validates values against the config rules, persists them and only
// then makes them current. Nothing changes unless every value is accepted.
// A non-empty ifMatch must be the current ETag ("*" matches any). It
// returns the settings whose value changed, sorted, and the new ETag.
func (s *SettingsSource) update(ctx context.Context, values map[string]string, ifMatch string) ([]string, string, error) {
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
	if ifMatch != "" && ifMatch != "*" && ifMatch != s.etag() {
		if s.changedOnDisk() {
			return nil, "", errChangedOnDisk()
		}
		return nil, "", withKind(ErrConfigChanged, errors.New("the settings changed after this page loaded them; reload the page to see them, then retry"))
	}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if by, ok := s.pinned[k]; ok && values[k] != s.values[k] {
			return nil, "", withKind(ErrConfigOverridden, fmt.Errorf("%s is set by %s; reinstall the service with `flats install` to manage it here", k, by))
		}
	}
	next := s.doc.Clone()
	if err := apply(next); err != nil {
		return nil, "", invalid(err)
	}
	if s.file != nil {
		h, err := s.file.Save(s.file.Hash, apply)
		if err != nil {
			if errors.Is(err, config.ErrConflict) {
				if ifMatch != "" {
					return nil, "", errChangedOnDisk()
				}
				return nil, "", withKind(ErrConflict, fmt.Errorf("config.json changed on disk since Flats loaded it; restart `flats serve` to load the file, then retry: %w", err))
			}
			var fe *config.FieldError
			if errors.As(err, &fe) {
				return nil, "", invalid(err)
			}
			if ifMatch != "" && s.changedOnDisk() {
				// The file the ETag names is gone or cannot be read.
				return nil, "", errChangedOnDisk()
			}
			return nil, "", err
		}
		s.file.Hash = h
	}
	prev, prevValues := s.doc, s.values
	s.doc = next
	if err := s.refresh(); err != nil {
		s.doc = prev
		return nil, "", err
	}
	var changed []string
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if s.values[k] != prevValues[k] {
			changed = append(changed, k)
		}
	}
	return changed, s.etag(), nil
}

func errChangedOnDisk() error {
	return withKind(ErrConfigChanged, errors.New("config.json changed on disk since Flats loaded it; restart Flats to load the file, then retry"))
}

// changedOnDisk reports whether the file on disk is not the one the host
// last read or wrote, including when it cannot be read. s.mu is held.
func (s *SettingsSource) changedOnDisk() bool {
	if s.file == nil || s.file.DiskHash == nil {
		return false
	}
	h, err := s.file.DiskHash()
	return err != nil || h != s.file.Hash
}

// ConfigView describes the config.json behind the settings for the
// console. It holds no paths and no credential contents.
type ConfigView struct {
	Mode          string `json:"mode"` // "config" or "legacy"
	SchemaVersion int    `json:"schema_version"`
	// ETag is the hash of the file the host last read or wrote.
	ETag string `json:"etag"`
	// ChangedOnDisk reports that the file differs from ETag (or cannot be
	// read); a restart loads it.
	ChangedOnDisk bool `json:"changed_on_disk"`
	Host          struct {
		ManagementAddr string `json:"management_addr"`
		LocalAddr      string `json:"local_addr"`
		ConsoleHost    string `json:"console_host"`
		ServerRuntime  bool   `json:"server_runtime"`
	} `json:"host"`
	Network struct {
		Permitted      []string `json:"permitted"`
		PrivateBackend string   `json:"private_backend"`
	} `json:"network"`
	// Credentials reports only whether each file is configured.
	Credentials struct {
		OperatorFile         bool `json:"operator_file"`
		TailscaleAuthKeyFile bool `json:"tailscale_authkey_file"`
	} `json:"credentials"`
	// Keys maps each setting to its config.json key.
	Keys map[string]string `json:"keys"`
	// Sources maps every config.json key to where its value comes from.
	Sources map[string]config.Source `json:"sources"`
	// Pinned maps settings the console cannot change to what sets them.
	Pinned map[string]string `json:"pinned"`
}

// view returns the ConfigView, or nil without a file.
func (s *SettingsSource) view() (*ConfigView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil, nil
	}
	c, err := s.doc.Effective(s.file.Overrides)
	if err != nil {
		return nil, err
	}
	v := &ConfigView{Mode: s.file.Mode, SchemaVersion: s.doc.SchemaVersion(), ETag: s.file.Hash, ChangedOnDisk: s.changedOnDisk(),
		Keys: maps.Clone(settingKeys), Sources: map[string]config.Source{}, Pinned: maps.Clone(s.pinned)}
	if v.Pinned == nil {
		v.Pinned = map[string]string{}
	}
	v.Host.ManagementAddr, v.Host.LocalAddr = c.Host.ManagementAddr, c.Host.LocalAddr
	v.Host.ConsoleHost, v.Host.ServerRuntime = c.Host.ConsoleHost, c.Host.ServerRuntime
	v.Network.Permitted, v.Network.PrivateBackend = c.Network.Permitted, c.Network.PrivateBackend
	v.Credentials.OperatorFile = c.Credentials.OperatorFile != ""
	v.Credentials.TailscaleAuthKeyFile = c.Credentials.TailscaleAuthKeyFile != ""
	for _, k := range config.Keys() {
		_, src, _ := c.Lookup(k)
		v.Sources[k] = src
	}
	return v, nil
}

func (s *Service) setting(key string) string { return s.settings.get(key) }

// clampInt converts a setting to int, saturating where int is narrower.
func clampInt(n int64) int {
	if n > math.MaxInt {
		return math.MaxInt
	}
	if n < math.MinInt {
		return math.MinInt
	}
	return int(n)
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
func (s *Service) keepVersions() int  { return clampInt(s.intSetting(SetKeepVersions)) }
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

// SettingsConfig describes the config.json behind the settings, or returns
// nil when the settings are not saved to a file.
func (s *Service) SettingsConfig() (*ConfigView, error) { return s.settings.view() }

// SettingsUpdate is the result of UpdateSettingsMatch.
type SettingsUpdate struct {
	Settings map[string]string
	// Changed lists the settings whose value changed, sorted.
	Changed []string
	// ETag is the new settings ETag ("" without a file).
	ETag string
}

// UpdateSettings validates and stores settings. Only the console calls it.
// Nothing is stored unless every value is valid. The Portal settings are
// read when `flats serve` starts.
func (s *Service) UpdateSettings(ctx context.Context, in map[string]string) (map[string]string, error) {
	u, err := s.UpdateSettingsMatch(ctx, in, "")
	return u.Settings, err
}

// UpdateSettingsMatch is UpdateSettings when the settings ETag is still
// ifMatch; a different one fails with ErrConfigChanged. An empty ifMatch
// skips the check.
func (s *Service) UpdateSettingsMatch(ctx context.Context, in map[string]string, ifMatch string) (SettingsUpdate, error) {
	clean, err := cleanSettings(in)
	if err != nil {
		return SettingsUpdate{}, err
	}
	changed, etag, err := s.settings.update(ctx, clean, ifMatch)
	if err != nil {
		if ErrorCategory(err) == "" {
			// A failed write names config.json's path; keep it in the log.
			s.logf("save settings to config.json: %v", err)
			return SettingsUpdate{}, errors.New("could not save config.json; the Flats log has the details")
		}
		return SettingsUpdate{}, err
	}
	if _, ok := clean[SetRateLimit]; ok {
		s.mu.Lock()
		for _, lf := range s.live {
			lf.limiter.setRate(s.rateLimit())
		}
		s.mu.Unlock()
	}
	all, _ := s.Settings(ctx)
	return SettingsUpdate{Settings: all, Changed: changed, ETag: etag}, nil
}

// cleanSettings validates and normalizes setting values.
func cleanSettings(in map[string]string) (map[string]string, error) {
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
	return clean, nil
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
