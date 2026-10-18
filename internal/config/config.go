// Package config reads, validates and writes the host configuration file
// (config.json).
//
// The file is sparse: it holds only values an operator chose. A key that is
// absent uses its frozen default from this package. A value written equal to
// its default stays in the file; Unset removes a key. Defaults never change
// after release: changing one is a breaking change that bumps
// CurrentSchemaVersion and adds a migration writing the old default into
// files that lacked the key. Adding a key with a default does not bump the
// version.
//
// The package does not read or stat credential files and does not take the
// data lock; callers serialize writers across processes.
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/portal-tunnel/v2/utils"
)

// CurrentSchemaVersion is the config.json format this binary reads and writes.
const CurrentSchemaVersion = 4

// Source tells where an effective value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceFlag    Source = "flag" // a per-run override, never stored
)

// Network providers that network.permitted may name, in canonical order.
// Local is always available and is not listed.
var providers = []string{"tailscale", "tailscale-funnel", "portal", "zrok", "cloudflare-quick", "netbird", "netbird-expose"}

type kind int

const (
	kindString kind = iota
	kindBool
	kindInt
	kindList
)

// spec describes one key. A spec without a default is either required or
// optional with no value ("none").
type spec struct {
	key      string
	kind     kind
	def      any // string, bool, int64 or []string; nil = no default
	required bool
	min, max int64                  // kindInt only
	check    func(any) (any, error) // validates and normalizes; nil = type only
	hint     string
	override bool // may be overridden for one run (SourceFlag)
	// deprecated keys are still read from a file but Set refuses them; the
	// host consumes and removes them.
	deprecated string
}

// sections lists the top-level objects in file order.
var sections = []string{"host", "network", "tailscale", "system", "portal", "zrok", "netbird", "console", "credentials"}

// specs lists every key in file order. The defaults are frozen.
var specs = []spec{
	{key: "host.instance_id", kind: kindString, required: true, check: checkUUID,
		hint: "use the lowercase UUID written by config init"},
	{key: "host.data_dir", kind: kindString, required: true, check: checkAbsPath,
		hint: "use an absolute path"},
	{key: "host.management_addr", kind: kindString, def: "127.0.0.1:7878", check: checkAddr, override: true,
		hint: "use host:port with a port from 0 to 65535, such as 127.0.0.1:7878"},
	{key: "host.local_addr", kind: kindString, def: "127.0.0.1:7879", check: checkAddr, override: true,
		hint: "use host:port with a port from 0 to 65535, such as 127.0.0.1:7879"},
	{key: "host.server_runtime", kind: kindBool, def: true, override: true,
		hint: "use true or false"},
	{key: "network.permitted", kind: kindList, def: []string{}, check: checkPermitted,
		hint: `list any of "tailscale", "tailscale-funnel", "portal", "zrok", "cloudflare-quick", "netbird" and "netbird-expose" once each; local is always available`},
	{key: "network.private_backend", kind: kindString, def: "local", check: checkBackend,
		hint: `use "local" or "tailscale"`},
	// The providers a new flat is allowed on from the start, one per mode.
	// Unset, the private default follows network.private_backend and new
	// flats get no public provider. A new flat is always Private: a public
	// default is only a permission, never exposure.
	{key: "network.default_private", kind: kindString, def: "", check: checkDefaultPrivate,
		hint: `use "local", "tailscale" or "netbird", or "" to follow network.private_backend`},
	{key: "network.default_public", kind: kindString, def: "", check: checkDefaultPublic,
		hint: `use "tailscale-funnel", "portal", "zrok", "cloudflare-quick" or "netbird-expose", or "" for none`},
	// The system limits accept every value up to 2^53-1 (exact in JSON
	// numbers everywhere), as the settings table did.
	// The console's tailnet names, in the order they were added. Each one is
	// its own tailnet node. A non-empty list runs the console on Tailscale.
	{key: "tailscale.console_hosts", kind: kindList, def: []string{}, check: checkConsoleHosts, override: true,
		hint: "list lowercase DNS labels such as flats once each, or [] to keep the console off the tailnet"},
	intSpec("system.upload_max_bytes", 20<<20, 1, MaxInt),
	intSpec("system.version_quota", 20, 0, MaxInt),
	intSpec("system.disk_quota_bytes", 30<<30, 0, MaxInt),
	intSpec("system.preview_ttl_seconds", 24*3600, 1, MaxInt),
	intSpec("system.rate_limit_rps", 50, 1, MaxInt),
	intSpec("system.redirect_days", 7, 0, MaxInt),
	intSpec("system.events_keep", 5000, 1, MaxInt),
	intSpec("system.revision_quota", 100, 0, MaxInt),
	intSpec("system.pin_quota", 20, 0, MaxInt),
	{key: "portal.relays", kind: kindList, def: []string{}, check: checkRelays,
		hint: "list https://host[:port] relay origins once each, or [] for Portal defaults"},
	{key: "portal.discovery", kind: kindBool, def: true,
		hint: "use true or false"},
	intSpec("portal.max_active_relays", 3, 1, 1<<31-1),
	// hide keeps public flats out of relay listings unless a flat overrides
	// it. It is not access control: the URL still opens the flat.
	{key: "portal.hide", kind: kindBool, def: false,
		hint: "use true or false"},
	// Unset, zrok uses the environment Flats manages in the data directory,
	// connected from the console with the account token. Set, it names an
	// environment managed with the zrok CLI.
	{key: "zrok.environment", kind: kindString, check: checkAbsPath,
		hint: "use the absolute path of a zrok environment directory, or unset the key to connect the account in the console"},
	{key: "zrok.namespace", kind: kindString, def: "public", check: checkNamespace,
		hint: "use a zrok namespace token such as public"},
	// The NetBird management server the host's peer registers with. The
	// default is NetBird Cloud; a self-hosted server sets its own URL.
	{key: "netbird.management_url", kind: kindString, def: "https://api.netbird.io:443", check: checkManagementURL,
		hint: "use the https URL of a NetBird management server, such as https://api.netbird.io:443"},
	// The console's NetBird names, in the order they were added. Each one is
	// its own NetBird peer.
	{key: "netbird.console_hosts", kind: kindList, def: []string{}, check: checkConsoleHosts,
		hint: "list lowercase DNS labels such as flats once each, or [] to keep the console off NetBird"},
	// Origins declared by the operator for reverse proxy access.
	{key: ExternalOriginsKey, kind: kindList, def: []string{}, check: checkExternalOrigins,
		hint: "list exact http:// or https:// DNS origins, without a path, once each"},
	// The console address links name when they cannot follow the request:
	// "local" (the loopback address), "tailscale:NAME" or "netbird:NAME"
	// with NAME in that console domain list, or "" for none. Set refuses a
	// NAME not in its list; a file edit that leaves one there is ignored
	// with a warning (PreferredHostProblem), not refused.
	{key: "console.preferred_host", kind: kindString, def: "", check: checkPreferredHost,
		hint: preferredHostHint},
	{key: "credentials.operator_file", kind: kindString, check: checkAbsPath,
		hint: "use an absolute path, or unset the key to turn console approval off"},
	// Read once at start into flats.db, then removed from the file.
	{key: "credentials.tailscale_authkey_file", kind: kindString, check: checkAbsPath,
		hint:       "unset it; register the key in the console, with `flats key set tailscale` or with `flats serve --tailscale-authkey-file`",
		deprecated: "the Tailscale auth key is kept in flats.db; register it in the console, with `flats key set tailscale` or with `flats serve --tailscale-authkey-file`"},
}

// MaxInt is the largest system.* value, 2^53-1.
const MaxInt = 1<<53 - 1

func intSpec(key string, def, lo, hi int64) spec {
	return spec{key: key, kind: kindInt, def: def, min: lo, max: hi,
		hint: fmt.Sprintf("use an integer from %d to %d", lo, hi)}
}

func lookupSpec(key string) (*spec, bool) {
	for i := range specs {
		if specs[i].key == key {
			return &specs[i], true
		}
	}
	return nil, false
}

// Default returns the frozen default of key: a string, bool, int64 or
// []string, or nil for a key without one.
func Default(key string) (any, bool) {
	sp, ok := lookupSpec(key)
	if !ok {
		return nil, false
	}
	if l, isList := sp.def.([]string); isList {
		return slices.Clone(l), true
	}
	return sp.def, true
}

// Keys returns every config key in file order.
func Keys() []string {
	keys := make([]string, len(specs))
	for i, s := range specs {
		keys[i] = s.key
	}
	return keys
}

// FieldError is a rejected value. Path is the JSON path, such as
// "system.keep_versions" or "network.permitted[1]".
type FieldError struct {
	Path string
	Msg  string
	Hint string
}

func (e *FieldError) Error() string {
	s := "config: " + e.Path + ": " + e.Msg
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}

func fieldErr(path, hint, format string, args ...any) *FieldError {
	return &FieldError{Path: path, Msg: fmt.Sprintf(format, args...), Hint: hint}
}

// Document is a sparse config.json: only the keys present in the file. A
// Document returned by Parse, Load or New is valid; Set and Unset validate
// one key, and Validate checks the rules that span keys.
type Document struct {
	version int
	values  map[string]any // key -> string, bool, int64 or []string
}

// New returns a minimal document for config init.
func New(instanceID, dataDir string) (*Document, error) {
	d := &Document{version: CurrentSchemaVersion, values: map[string]any{}}
	if err := d.Set("host.instance_id", instanceID); err != nil {
		return nil, err
	}
	if err := d.Set("host.data_dir", dataDir); err != nil {
		return nil, err
	}
	return d, nil
}

// Clone returns an independent copy of d.
func (d *Document) Clone() *Document {
	c := &Document{version: d.version, values: make(map[string]any, len(d.values))}
	for k, v := range d.values {
		if l, isList := v.([]string); isList {
			v = slices.Clone(l)
		}
		c.values[k] = v
	}
	return c
}

// Stored returns the value key holds in the document itself (no default).
func (d *Document) Stored(key string) (any, bool) {
	v, ok := d.values[key]
	return v, ok
}

// SchemaVersion is the document's schema_version.
func (d *Document) SchemaVersion() int { return d.version }

// Set parses value by the key's type and stores it, keeping it even when it
// equals the default. Integers use decimal digits only; booleans are "true"
// or "false"; lists are a JSON array (`["a","b"]`) or comma-separated items
// (`a, b`), where blank items are skipped and "" is the empty list.
func (d *Document) Set(key, value string) error {
	sp, ok := lookupSpec(key)
	if !ok {
		return unknownKey(key)
	}
	if sp.deprecated != "" {
		return fieldErr(key, sp.hint, "can no longer be set: %s", sp.deprecated)
	}
	v, err := sp.parseString(value)
	if err != nil {
		return err
	}
	if key == PreferredHostKey {
		if err := d.checkPreferredListed(v.(string)); err != nil {
			return err
		}
	}
	before := d.consoleList(key)
	d.values[key] = v
	d.dropRemovedPreference(key, before)
	return nil
}

// SetDeprecated stores a deprecated key, which Set refuses. Only a
// conversion of older inputs uses it, such as the migration of a legacy
// service's --authkey-file; the host consumes and removes the key.
func (d *Document) SetDeprecated(key, value string) error {
	sp, ok := lookupSpec(key)
	if !ok {
		return unknownKey(key)
	}
	v, err := sp.parseString(value)
	if err != nil {
		return err
	}
	d.values[key] = v
	return nil
}

// Unset removes key so its default applies. Required keys cannot be unset.
// Removing an absent key is not an error.
func (d *Document) Unset(key string) error {
	sp, ok := lookupSpec(key)
	if !ok {
		return unknownKey(key)
	}
	if sp.required {
		return fieldErr(key, "set it to a new value instead", "is required and cannot be unset")
	}
	before := d.consoleList(key)
	delete(d.values, key)
	d.dropRemovedPreference(key, before)
	return nil
}

func unknownKey(key string) *FieldError {
	return fieldErr(key, "run `flats config show --effective` for the key list", "unknown key")
}

// Validate checks required keys and the rules that span keys.
func (d *Document) Validate() error {
	_, err := d.Effective(nil)
	return err
}

// parseString parses a CLI or override value.
func (sp *spec) parseString(s string) (any, error) {
	if !utf8.ValidString(s) {
		return nil, fieldErr(sp.key, sp.hint, "value is not valid UTF-8")
	}
	var v any
	switch sp.kind {
	case kindString:
		v = s
	case kindBool:
		switch s {
		case "true":
			v = true
		case "false":
			v = false
		default:
			return nil, fieldErr(sp.key, sp.hint, "%q is not a boolean", s)
		}
	case kindInt:
		if !isIntSyntax(s) {
			return nil, fieldErr(sp.key, sp.hint, "%q is not an integer", s)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fieldErr(sp.key, sp.hint, "%s is out of range", s)
		}
		v = n
	case kindList:
		if t := strings.TrimSpace(s); strings.HasPrefix(t, "[") {
			raw, err := decodeValue([]byte(t), sp.key)
			if err != nil {
				return nil, err
			}
			return sp.fromRaw(raw, sp.key)
		}
		list := []string{}
		for part := range strings.SplitSeq(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				list = append(list, part)
			}
		}
		v = list
	}
	return sp.validate(v, sp.key)
}

// validate range-checks and normalizes a typed value.
func (sp *spec) validate(v any, path string) (any, error) {
	if sp.kind == kindInt {
		if n := v.(int64); n < sp.min || n > sp.max {
			return nil, fieldErr(path, sp.hint, "%d is out of range", n)
		}
	}
	if sp.check == nil {
		return v, nil
	}
	norm, err := sp.check(v)
	if err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return nil, err
		}
		return nil, fieldErr(path, sp.hint, "%v", err)
	}
	return norm, nil
}

func checkUUID(v any) (any, error) {
	s := v.(string)
	if len(s) != 36 {
		return nil, fmt.Errorf("%q is not a UUID", s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return nil, fmt.Errorf("%q is not a UUID", s)
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return nil, fmt.Errorf("%q is not a lowercase UUID", s)
		}
	}
	return s, nil
}

// NewInstanceID returns a random (version 4) UUID for host.instance_id.
func NewInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func checkAbsPath(v any) (any, error) {
	s := v.(string)
	if strings.IndexByte(s, 0) >= 0 {
		return nil, errors.New("path contains a NUL byte")
	}
	if !filepath.IsAbs(s) {
		return nil, fmt.Errorf("%q is not an absolute path", s)
	}
	return s, nil
}

// checkAddr accepts every address `flats serve` could listen on: any
// host (an IP, a name or empty) and a decimal port from 0 to 65535, where an
// empty port or 0 picks a free port. The value is kept as written.
func checkAddr(v any) (any, error) {
	s := v.(string)
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not a host:port address", s)
	}
	if _, err := strconv.ParseUint(port, 10, 16); port != "" && err != nil {
		return nil, fmt.Errorf("port %q is not a number from 0 to 65535", port)
	}
	return s, nil
}

// ephemeralPort reports whether addr picks a free port.
func ephemeralPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && strings.TrimLeft(port, "0") == ""
}

func checkDefaultPrivate(v any) (any, error) {
	switch s := v.(string); s {
	case "", "local", "tailscale", "netbird":
		return s, nil
	default:
		return nil, fmt.Errorf("%q is not a private provider", s)
	}
}

func checkDefaultPublic(v any) (any, error) {
	switch s := v.(string); s {
	case "", "tailscale-funnel", "portal", "zrok", "cloudflare-quick", "netbird-expose":
		return s, nil
	default:
		return nil, fmt.Errorf("%q is not a public provider", s)
	}
}

func checkBackend(v any) (any, error) {
	switch s := v.(string); s {
	case "local", "tailscale":
		return s, nil
	default:
		return nil, fmt.Errorf("%q is not a private backend", s)
	}
}

// checkPermitted rejects unknown, local and repeated providers and returns
// them in canonical order.
func checkPermitted(v any) (any, error) {
	list := v.([]string)
	for i, p := range list {
		path := fmt.Sprintf("network.permitted[%d]", i)
		if p == "local" {
			return nil, fieldErr(path, "remove it; local is always available", "local is not a grant")
		}
		if !slices.Contains(providers, p) {
			return nil, fieldErr(path, `use "tailscale", "tailscale-funnel", "portal", "zrok", "cloudflare-quick", "netbird" or "netbird-expose"`, "unknown provider %q", p)
		}
		if slices.Contains(list[:i], p) {
			return nil, fieldErr(path, "list each provider once", "duplicate provider %q", p)
		}
	}
	out := []string{}
	for _, p := range providers {
		if slices.Contains(list, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ConsoleHostRE is a lowercase DNS label, the tailnet host names tsnet
// accepts.
var ConsoleHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkConsoleHosts accepts lowercase DNS labels, each once, in order.
func checkConsoleHosts(v any) (any, error) {
	list := v.([]string)
	for i, name := range list {
		path := fmt.Sprintf("tailscale.console_hosts[%d]", i)
		if !ConsoleHostRE.MatchString(name) {
			return nil, fieldErr(path, "use 1 to 63 lowercase letters, digits and '-', not starting or ending with '-'", "%q is not a lowercase DNS label", name)
		}
		if slices.Contains(list[:i], name) {
			return nil, fieldErr(path, "list each name once", "duplicate console host %q", name)
		}
	}
	return slices.Clone(list), nil
}

// PreferredHostKey is the console address links use when they cannot
// follow the request.
const PreferredHostKey = "console.preferred_host"

const preferredHostHint = `use "local", "tailscale:NAME" or "netbird:NAME" with NAME in tailscale.console_hosts or netbird.console_hosts, or unset it for no preference`

// consoleListKeys maps each console domain network to its list key.
var consoleListKeys = map[string]string{"tailscale": "tailscale.console_hosts", "netbird": "netbird.console_hosts"}

// ParsePreferredHost splits a console.preferred_host value. It returns
// ("local", "") for the loopback address and (network, name) for a console
// domain; ok is false for "" (no preference) and malformed values.
func ParsePreferredHost(v string) (network, name string, ok bool) {
	if v == "local" {
		return "local", "", true
	}
	network, name, found := strings.Cut(v, ":")
	if !found || consoleListKeys[network] == "" || !ConsoleHostRE.MatchString(name) {
		return "", "", false
	}
	return network, name, true
}

// checkPreferredHost accepts the forms of console.preferred_host; whether
// NAME is listed is checked by Set (checkPreferredListed).
func checkPreferredHost(v any) (any, error) {
	s := v.(string)
	if s == "" {
		return s, nil
	}
	if _, _, ok := ParsePreferredHost(s); !ok {
		return nil, fmt.Errorf("%q is not a console address", s)
	}
	return s, nil
}

// consoleList returns the console domain list of a list key as the document
// holds it (the default when unset), or nil for any other key.
func (d *Document) consoleList(key string) []string {
	for _, k := range consoleListKeys {
		if k == key {
			if v, ok := d.values[key].([]string); ok {
				return v
			}
			return []string{}
		}
	}
	return nil
}

// checkPreferredListed refuses a preference whose NAME is not in its
// network's console domain list in this document.
func (d *Document) checkPreferredListed(v string) error {
	network, name, ok := ParsePreferredHost(v)
	if !ok || network == "local" {
		return nil
	}
	key := consoleListKeys[network]
	if !slices.Contains(d.consoleList(key), name) {
		return fieldErr(PreferredHostKey, fmt.Sprintf("add %s to %s first, or choose a listed name", name, key),
			"%q is not in %s", name, key)
	}
	return nil
}

// dropRemovedPreference unsets console.preferred_host when a change of the
// console domain list key removed the name it prefers, so deleting the
// preferred domain unsets the preference in the same change. A preference
// that names a name the list did not hold before (a file edit) is left alone.
func (d *Document) dropRemovedPreference(key string, before []string) {
	if before == nil {
		return
	}
	pref, _ := d.values[PreferredHostKey].(string)
	network, name, ok := ParsePreferredHost(pref)
	if !ok || consoleListKeys[network] != key {
		return
	}
	if slices.Contains(before, name) && !slices.Contains(d.consoleList(key), name) {
		delete(d.values, PreferredHostKey)
	}
}

// checkManagementURL accepts an https URL without credentials, query or
// fragment. A NetBird setup key is never part of it.
func checkManagementURL(v any) (any, error) {
	s := v.(string)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return nil, fmt.Errorf("%q is not an https URL", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%q must be only scheme, host and port", s)
	}
	return s, nil
}

// checkNamespace accepts a zrok namespace token: letters, digits, '-' and
// '_', up to 64 characters.
func checkNamespace(v any) (any, error) {
	s := v.(string)
	if s == "" || len(s) > 64 {
		return nil, fmt.Errorf("namespace %q must be 1 to 64 characters", s)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return nil, fmt.Errorf("namespace %q may contain only letters, digits, '-' and '_'", s)
		}
	}
	return s, nil
}

// checkRelays normalizes each relay like the Portal CLI and rejects
// duplicates after normalization. Order is kept.
func checkRelays(v any) (any, error) {
	list := v.([]string)
	out := make([]string, 0, len(list))
	for i, r := range list {
		path := fmt.Sprintf("portal.relays[%d]", i)
		u, err := utils.NormalizeRelayURL(r)
		if err != nil {
			return nil, fieldErr(path, "use https://host[:port]", "relay %q: %v", r, err)
		}
		if slices.Contains(out, u) {
			return nil, fieldErr(path, "list each relay once", "duplicate relay %q", u)
		}
		out = append(out, u)
	}
	return out, nil
}

// Config is the effective configuration: defaults, file values and per-run
// overrides resolved into typed fields.
type Config struct {
	SchemaVersion int
	Host          HostConfig
	Network       NetworkConfig
	Tailscale     TailscaleConfig
	System        SystemConfig
	Portal        PortalConfig
	Zrok          ZrokConfig
	Netbird       NetbirdConfig
	Console       ConsoleConfig
	Credentials   CredentialsConfig

	values  map[string]any
	sources map[string]Source
}

type HostConfig struct {
	InstanceID     string
	DataDir        string
	ManagementAddr string
	LocalAddr      string
	ServerRuntime  bool
}

type TailscaleConfig struct {
	ConsoleHosts []string // the console's tailnet names, in order
}

type NetworkConfig struct {
	Permitted      []string // canonical order; never contains "local"
	PrivateBackend string   // "local" or "tailscale"
	DefaultPrivate string   // "", "local" or "tailscale"; "" follows PrivateBackend
	DefaultPublic  string   // "" or a public provider in Permitted
}

// NewFlatProviders returns the providers a new flat is allowed on: the
// effective private default (unless it is local, which needs no grant) and
// the public default, if any.
func (n NetworkConfig) NewFlatProviders() []string {
	var out []string
	if p := n.EffectiveDefaultPrivate(); p != "local" {
		out = append(out, p)
	}
	if n.DefaultPublic != "" {
		out = append(out, n.DefaultPublic)
	}
	return out
}

// EffectiveDefaultPrivate is DefaultPrivate, or PrivateBackend when unset.
func (n NetworkConfig) EffectiveDefaultPrivate() string {
	if n.DefaultPrivate != "" {
		return n.DefaultPrivate
	}
	return n.PrivateBackend
}

type SystemConfig struct {
	UploadMaxBytes    int64
	VersionQuota      int64 // 0 = unlimited
	DiskQuotaBytes    int64 // 0 = off
	PreviewTTLSeconds int64
	RateLimitRPS      int64
	RedirectDays      int64
	EventsKeep        int64
}

type PortalConfig struct {
	Relays          []string // empty = Portal defaults
	Discovery       bool
	MaxActiveRelays int64
	Hide            bool // default relay listing of public flats
}

type NetbirdConfig struct {
	ManagementURL string
	ConsoleHosts  []string // the console's NetBird names, in order
}

// ConsoleConfig is the console section.
type ConsoleConfig struct {
	ExternalOrigins []string

	// PreferredHost is console.preferred_host: "", "local",
	// "tailscale:NAME" or "netbird:NAME". Use Preferred for the value in
	// effect, which ignores a NAME not in its list.
	PreferredHost string
}

// Preferred returns the console address links prefer when they cannot
// follow the request: network "local", or "tailscale" or "netbird" with a
// NAME listed in that network's console domains. ok is false without a
// preference, and for a preference whose NAME is not listed (see
// PreferredHostProblem).
func (c *Config) Preferred() (network, name string, ok bool) {
	network, name, ok = ParsePreferredHost(c.Console.PreferredHost)
	if !ok || network == "local" {
		return network, name, ok
	}
	if !slices.Contains(c.ConsoleHostsOf(network), name) {
		return "", "", false
	}
	return network, name, true
}

// ConsoleHostsOf returns the console domains of network ("tailscale" or
// "netbird").
func (c *Config) ConsoleHostsOf(network string) []string {
	switch network {
	case "tailscale":
		return slices.Clone(c.Tailscale.ConsoleHosts)
	case "netbird":
		return slices.Clone(c.Netbird.ConsoleHosts)
	}
	return nil
}

// PreferredHostProblem explains why console.preferred_host is ignored: it
// names a console domain its list does not hold (left by a file edit, or by
// a --console-host flag that replaces the list for one run). It returns ""
// when the preference is unset or in effect.
func (c *Config) PreferredHostProblem() string {
	pref := c.Console.PreferredHost
	if _, _, ok := ParsePreferredHost(pref); !ok || pref == "local" {
		return ""
	}
	if _, _, ok := c.Preferred(); ok {
		return ""
	}
	network, name, _ := ParsePreferredHost(pref)
	return fmt.Sprintf("%s %q names %s, which is not in %s; it is ignored, and links use the first served console domain (Tailscale, then NetBird), else the loopback address",
		PreferredHostKey, pref, name, consoleListKeys[network])
}

type ZrokConfig struct {
	Environment string // "" = the zrok default, ~/.zrok2
	Namespace   string
}

// CredentialsConfig holds paths only; "" means none.
type CredentialsConfig struct {
	OperatorFile         string
	TailscaleAuthKeyFile string
}

// Lookup returns the effective value of key (string, bool, int64 or
// []string) and its source. A key with no default and no value has a nil
// value and SourceDefault.
func (c *Config) Lookup(key string) (value any, src Source, ok bool) {
	if _, ok := lookupSpec(key); !ok {
		return nil, "", false
	}
	value = c.values[key]
	if l, isList := value.([]string); isList {
		value = slices.Clone(l)
	}
	return value, c.sources[key], true
}

// Effective resolves the document with defaults and per-run overrides.
// Only host.management_addr, host.local_addr, host.server_runtime and
// tailscale.console_hosts may be overridden; the values are parsed like Set.
func (d *Document) Effective(overrides map[string]string) (*Config, error) {
	c := &Config{SchemaVersion: d.version, values: map[string]any{}, sources: map[string]Source{}}
	var errs []error
	for _, sp := range specs {
		if v, ok := d.values[sp.key]; ok {
			c.values[sp.key], c.sources[sp.key] = v, SourceFile
		} else if sp.required {
			errs = append(errs, fieldErr(sp.key, "run `flats config init` or restore the key", "is required"))
		} else {
			c.values[sp.key], c.sources[sp.key] = sp.def, SourceDefault
		}
	}
	for _, key := range sortedKeys(overrides) {
		sp, ok := lookupSpec(key)
		if !ok {
			errs = append(errs, unknownKey(key))
			continue
		}
		if !sp.override {
			errs = append(errs, fieldErr(key, "change it in config.json", "cannot be overridden for one run"))
			continue
		}
		v, err := sp.parseString(overrides[key])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.values[key], c.sources[key] = v, SourceFlag
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := c.fill(); err != nil {
		return nil, err
	}
	return c, nil
}

// cleanDataDir returns host.data_dir in clean form. The key is validated as an
// absolute path, and Flats runs on Unix only, so the leading "/" changes
// nothing; filepath.Clean("/" + p) is also the form static analysis accepts
// as free of ".." traversal for the paths Flats builds under the directory.
func cleanDataDir(p string) string { return filepath.Clean("/" + p) }

// fill copies values into the typed fields and checks cross-key rules.
func (c *Config) fill() error {
	str := func(k string) string { s, _ := c.values[k].(string); return s }
	num := func(k string) int64 { return c.values[k].(int64) }
	flag := func(k string) bool { return c.values[k].(bool) }
	list := func(k string) []string { return slices.Clone(c.values[k].([]string)) }

	c.Host = HostConfig{str("host.instance_id"), cleanDataDir(str("host.data_dir")), str("host.management_addr"),
		str("host.local_addr"), flag("host.server_runtime")}
	c.Tailscale = TailscaleConfig{list("tailscale.console_hosts")}
	c.Network = NetworkConfig{list("network.permitted"), str("network.private_backend"),
		str("network.default_private"), str("network.default_public")}
	c.System = SystemConfig{num("system.upload_max_bytes"), num("system.version_quota"),
		num("system.disk_quota_bytes"), num("system.preview_ttl_seconds"), num("system.rate_limit_rps"),
		num("system.redirect_days"), num("system.events_keep")}
	c.Portal = PortalConfig{list("portal.relays"), flag("portal.discovery"), num("portal.max_active_relays"), flag("portal.hide")}
	c.Zrok = ZrokConfig{str("zrok.environment"), str("zrok.namespace")}
	c.Netbird = NetbirdConfig{str("netbird.management_url"), list("netbird.console_hosts")}
	c.Console = ConsoleConfig{PreferredHost: str(PreferredHostKey), ExternalOrigins: list(ExternalOriginsKey)}
	c.Credentials = CredentialsConfig{str("credentials.operator_file"), str("credentials.tailscale_authkey_file")}

	var errs []error
	// Two free ports never collide.
	if c.Host.ManagementAddr == c.Host.LocalAddr && !ephemeralPort(c.Host.LocalAddr) {
		errs = append(errs, fieldErr("host.local_addr", "use a port different from host.management_addr",
			"%s is also host.management_addr", c.Host.LocalAddr))
	}
	if c.Network.PrivateBackend == "tailscale" && !slices.Contains(c.Network.Permitted, "tailscale") {
		errs = append(errs, fieldErr("network.private_backend", `add "tailscale" to network.permitted or use "local"`,
			"tailscale is not permitted"))
	}
	if len(c.Tailscale.ConsoleHosts) > 0 && !slices.Contains(c.Network.Permitted, "tailscale") {
		errs = append(errs, fieldErr("tailscale.console_hosts", `add "tailscale" to network.permitted or remove the console hosts`,
			"tailscale is not permitted"))
	}
	if len(c.Netbird.ConsoleHosts) > 0 && !slices.Contains(c.Network.Permitted, "netbird") {
		errs = append(errs, fieldErr("netbird.console_hosts", `add "netbird" to network.permitted or remove the console hosts`,
			"netbird is not permitted"))
	}
	if p := c.Network.DefaultPrivate; (p == "tailscale" || p == "netbird") && !slices.Contains(c.Network.Permitted, p) {
		errs = append(errs, fieldErr("network.default_private", fmt.Sprintf(`add %q to network.permitted or use "local"`, p),
			"%s is not permitted", p))
	}
	if p := c.Network.DefaultPublic; p != "" && !slices.Contains(c.Network.Permitted, p) {
		errs = append(errs, fieldErr("network.default_public", fmt.Sprintf(`add %q to network.permitted or clear the default`, p),
			"%s is not permitted", p))
	}
	if !c.Portal.Discovery && len(c.Portal.Relays) == 0 {
		errs = append(errs, fieldErr("portal.discovery", "list portal.relays or set discovery to true",
			"false needs at least one relay"))
	}
	return errors.Join(errs...)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
