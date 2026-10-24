package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/config"
)

// restartKeys are the config.json keys a reload adopts but only the next
// start applies. network.permitted and the console domain lists reconcile
// live instead, and the host identity keys never change on a live host: a
// reload refuses a file that changes them.
var restartKeys = []string{
	"host.console_addr", "host.local_addr", "host.server_runtime",
	"metrics.enabled", "metrics.expose_on", "metrics.per_flat",
	"network.private_backend",
	"portal.relays", "portal.discovery", "portal.max_active_relays",
	"zrok.environment", "zrok.namespace", "netbird.management_url",
	"proxy.addr", authKeyFileKey,
}

// periodicSettings are the settings a reload adopts for their next occasion:
// the next sweep, the next pruning, new renames.
var periodicSettings = []string{SetPreviewTTL, SetEventsKeep, SetRedirectDays}

// overrideBy names the service flag behind each per-run override key.
var overrideBy = map[string]string{
	"host.console_addr":   "the service's --listen flag",
	"host.local_addr":     "the service's --local-addr flag",
	"host.server_runtime": "the service's --runtime flag",
	consoleHostsKey:       "the service's --console-host flag",
}

// ReloadHeld is a key whose adopted file value a run override or pin keeps
// from taking effect in this run.
type ReloadHeld struct {
	Key string `json:"key"`
	By  string `json:"by"`
}

// ConfigReload is the outcome of Service.ReloadConfig: config.json became the
// host's current truth. Keys are named, never valued.
type ConfigReload struct {
	// Applied lists the settings that changed and took effect at once.
	Applied []string
	// Periodic lists the changed settings that govern their next occasion.
	Periodic []string
	// AwaitRestart lists the changed config.json keys only the next start
	// applies.
	AwaitRestart []string
	// Held lists the keys whose differing file value a run override or pin
	// keeps from this run.
	Held []ReloadHeld
	// ETag is the settings ETag after the reload: the file's hash.
	ETag string
	// PermittedBefore and PermittedAfter are network.permitted before and
	// after the reload.
	PermittedBefore, PermittedAfter []string
	// The console domain lists in effect for this run, before and after. A
	// list a run override or pin holds is unchanged by the reload.
	TailscaleHostsBefore, TailscaleHostsAfter []string
	NetbirdHostsBefore, NetbirdHostsAfter     []string
	// PreferredProblem is why an adopted console.preferred_host is not in
	// effect, as a start logs it, or "".
	PreferredProblem string
}

// reload adopts the config.json file as the current truth, or refuses and
// changes nothing. s.mu is held for the whole adoption, as for a save.
func (s *SettingsSource) reload() (*ConfigReload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil || s.file.Load == nil {
		return nil, errors.New("the settings are not saved to a config.json file")
	}
	l, err := s.file.Load()
	if err != nil {
		return nil, err
	}
	if v := l.FileVersion; v != s.doc.SchemaVersion() {
		return nil, fmt.Errorf("schema_version is %d, but the running host uses %d; a reload cannot migrate a running host, so restart instead", v, s.doc.SchemaVersion())
	}
	for _, k := range []string{"host.instance_id", "host.data_dir"} {
		cur, _ := s.doc.Stored(k)
		next, _ := l.Doc.Stored(k)
		if fmt.Sprint(next) != fmt.Sprint(cur) {
			return nil, fmt.Errorf("%s differs from the running host's; it never changes on a live host", k)
		}
	}
	oldPlain, err := s.doc.Effective(nil)
	if err != nil {
		return nil, err
	}
	oldRun, err := s.doc.Effective(s.file.Overrides)
	if err != nil {
		return nil, err
	}
	newPlain, err := l.Doc.Effective(nil)
	if err != nil {
		return nil, err
	}
	// A start applies the same per-run overrides; a file they leave invalid
	// is one this run could not have started with.
	newRun, err := l.Doc.Effective(s.file.Overrides)
	if err != nil {
		return nil, err
	}

	rep := &ConfigReload{ETag: l.Hash,
		PermittedBefore: slices.Clone(oldPlain.Network.Permitted), PermittedAfter: slices.Clone(newPlain.Network.Permitted),
		TailscaleHostsBefore: slices.Clone(oldRun.Tailscale.ConsoleHosts), TailscaleHostsAfter: slices.Clone(newRun.Tailscale.ConsoleHosts),
		NetbirdHostsBefore: slices.Clone(oldRun.Netbird.ConsoleHosts), NetbirdHostsAfter: slices.Clone(newRun.Netbird.ConsoleHosts),
	}
	// The preference warning belongs to a reload that touched it, as a start
	// logs it once.
	if lookupString(newPlain, config.PreferredHostKey) != lookupString(oldPlain, config.PreferredHostKey) ||
		!slices.Equal(rep.TailscaleHostsBefore, rep.TailscaleHostsAfter) ||
		!slices.Equal(rep.NetbirdHostsBefore, rep.NetbirdHostsAfter) {
		rep.PreferredProblem = newRun.PreferredHostProblem()
	}

	// Run overrides and pins keep this run's value; a changed file value
	// enters the adopted document without changing this run.
	heldBy := map[string]string{}
	for key := range s.file.Overrides {
		by, ok := overrideBy[key]
		if !ok {
			by = "a per-run override"
		}
		heldBy[key] = by
	}
	for setting, by := range s.pinned {
		key := setting
		if ck, ok := settingKeys[setting]; ok {
			key = ck
		}
		heldBy[key] = by
	}
	for _, key := range slices.Sorted(maps.Keys(heldBy)) {
		if lookupString(newPlain, key) != lookupString(oldPlain, key) &&
			lookupString(newPlain, key) != lookupString(oldRun, key) {
			rep.Held = append(rep.Held, ReloadHeld{Key: key, By: heldBy[key]})
		}
	}
	if _, held := heldBy[consoleHostsKey]; held {
		rep.TailscaleHostsAfter = slices.Clone(rep.TailscaleHostsBefore)
	}
	for _, key := range restartKeys {
		if _, held := heldBy[key]; held {
			continue
		}
		if lookupString(newPlain, key) != lookupString(oldPlain, key) {
			rep.AwaitRestart = append(rep.AwaitRestart, key)
		}
	}

	prevDoc, prevValues, prevHash := s.doc, s.values, s.file.Hash
	s.doc = l.Doc
	s.file.Hash = l.Hash
	if err := s.refresh(); err != nil {
		s.doc, s.values, s.file.Hash = prevDoc, prevValues, prevHash
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(settingKeys)) {
		if s.values[k] == prevValues[k] {
			continue
		}
		if _, held := heldBy[settingKeys[k]]; held {
			continue
		}
		switch {
		case slices.Contains(periodicSettings, k):
			rep.Periodic = append(rep.Periodic, k)
		case slices.Contains(restartKeys, settingKeys[k]):
			// Already in AwaitRestart.
		default:
			rep.Applied = append(rep.Applied, k)
		}
	}
	return rep, nil
}

// lookupString formats key's effective value for comparison.
func lookupString(c *config.Config, key string) string {
	v, _, _ := c.Lookup(key)
	if v == nil {
		return ""
	}
	return settingString(v)
}

// ReloadConfig adopts config.json again, as a SIGHUP asks: the whole file is
// validated and then becomes the host's current truth, or the reload is
// refused and nothing changes. Changed keys with a live path take effect with
// the same side effects as a console settings save; a failed side effect is
// logged and the adopted file stands. As at a start after an offline edit,
// boards anyone can edit that a lowered quota no longer allows go back to
// view only. The report names every key by name only.
func (s *Service) ReloadConfig(ctx context.Context) (*ConfigReload, error) {
	s.publicEditMu.Lock()
	defer s.publicEditMu.Unlock()
	rep, err := s.settings.reload()
	if err != nil {
		return nil, err
	}
	if slices.Contains(rep.Applied, SetRateLimit) {
		s.mu.Lock()
		for _, lf := range s.live {
			lf.limiter.setRate(s.rateLimit())
		}
		s.mu.Unlock()
	}
	if slices.Contains(rep.Applied, SetPortalHide) {
		s.applyHostListing(ctx)
	}
	quotaChanged := false
	if slices.Contains(rep.Applied, SetRevisionQuota) {
		s.retainAllRevisions(ctx, true)
		quotaChanged = true
	}
	if slices.Contains(rep.Applied, SetDiskQuotaBytes) {
		s.pushAllDocsBudgets(ctx)
		s.remeasureBoards()
		quotaChanged = true
	}
	if quotaChanged {
		if err := s.downgradeEditBoards(ctx); err != nil {
			s.logf("config reload: downgrade boards anyone can edit: %v", err)
		}
	}
	return rep, nil
}
