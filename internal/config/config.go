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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/portal-tunnel/v2/utils"
)

// CurrentSchemaVersion is the config.json format this binary reads and writes.
const CurrentSchemaVersion = 1

// Source tells where an effective value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceFlag    Source = "flag" // a per-run override, never stored
)

// Network providers that network.permitted may name, in canonical order.
// Local is always available and is not listed.
var providers = []string{"tailscale", "tailscale-funnel", "portal", "zrok"}

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
}

// sections lists the top-level objects in file order.
var sections = []string{"host", "network", "system", "portal", "zrok", "credentials"}

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
	// Any console host `flats serve --console-host` accepted stays valid;
	// serve warns about one that is not a valid flat host name.
	{key: "host.console_host", kind: kindString, def: "flats", override: true,
		hint: "use a host name such as flats"},
	{key: "host.server_runtime", kind: kindBool, def: true, override: true,
		hint: "use true or false"},
	{key: "network.permitted", kind: kindList, def: []string{}, check: checkPermitted,
		hint: `list any of "tailscale", "tailscale-funnel", "portal" and "zrok" once each; local is always available`},
	{key: "network.private_backend", kind: kindString, def: "local", check: checkBackend,
		hint: `use "local" or "tailscale"`},
	// The system limits accept every value up to 2^53-1 (exact in JSON
	// numbers everywhere), as the settings table did.
	intSpec("system.upload_max_bytes", 20<<20, 1, MaxInt),
	intSpec("system.keep_versions", 10, 0, MaxInt),
	intSpec("system.disk_quota_bytes", 30<<30, 0, MaxInt),
	intSpec("system.preview_ttl_seconds", 24*3600, 1, MaxInt),
	intSpec("system.rate_limit_rps", 50, 1, MaxInt),
	intSpec("system.redirect_days", 7, 0, MaxInt),
	intSpec("system.events_keep", 5000, 1, MaxInt),
	{key: "portal.relays", kind: kindList, def: []string{}, check: checkRelays,
		hint: "list https://host[:port] relay origins once each, or [] for Portal defaults"},
	{key: "portal.discovery", kind: kindBool, def: true,
		hint: "use true or false"},
	intSpec("portal.max_active_relays", 3, 1, 1<<31-1),
	// hide keeps public flats out of relay listings unless a flat overrides
	// it. It is not access control: the URL still opens the flat.
	{key: "portal.hide", kind: kindBool, def: false,
		hint: "use true or false"},
	// zrok reads an environment the operator enabled with `zrok2 enable`;
	// Flats never stores the zrok account token.
	{key: "zrok.environment", kind: kindString, check: checkAbsPath,
		hint: "use the absolute path of a zrok environment directory, or unset the key for ~/.zrok2"},
	{key: "zrok.namespace", kind: kindString, def: "public", check: checkNamespace,
		hint: "use a zrok namespace token such as public"},
	{key: "credentials.operator_file", kind: kindString, check: checkAbsPath,
		hint: "use an absolute path, or unset the key to turn console approval off"},
	{key: "credentials.tailscale_authkey_file", kind: kindString, check: checkAbsPath,
		hint: "use an absolute path, or unset the key to log in interactively"},
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
	delete(d.values, key)
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
			return nil, fieldErr(path, `use "tailscale", "tailscale-funnel", "portal" or "zrok"`, "unknown provider %q", p)
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
	System        SystemConfig
	Portal        PortalConfig
	Zrok          ZrokConfig
	Credentials   CredentialsConfig

	values  map[string]any
	sources map[string]Source
}

type HostConfig struct {
	InstanceID     string
	DataDir        string
	ManagementAddr string
	LocalAddr      string
	ConsoleHost    string
	ServerRuntime  bool
}

type NetworkConfig struct {
	Permitted      []string // canonical order; never contains "local"
	PrivateBackend string   // "local" or "tailscale"
}

type SystemConfig struct {
	UploadMaxBytes    int64
	KeepVersions      int64 // 0 = no pruning
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
// Only host.management_addr, host.local_addr, host.console_host and
// host.server_runtime may be overridden; the values are parsed like Set.
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

// fill copies values into the typed fields and checks cross-key rules.
func (c *Config) fill() error {
	str := func(k string) string { s, _ := c.values[k].(string); return s }
	num := func(k string) int64 { return c.values[k].(int64) }
	flag := func(k string) bool { return c.values[k].(bool) }
	list := func(k string) []string { return slices.Clone(c.values[k].([]string)) }

	c.Host = HostConfig{str("host.instance_id"), str("host.data_dir"), str("host.management_addr"),
		str("host.local_addr"), str("host.console_host"), flag("host.server_runtime")}
	c.Network = NetworkConfig{list("network.permitted"), str("network.private_backend")}
	c.System = SystemConfig{num("system.upload_max_bytes"), num("system.keep_versions"),
		num("system.disk_quota_bytes"), num("system.preview_ttl_seconds"), num("system.rate_limit_rps"),
		num("system.redirect_days"), num("system.events_keep")}
	c.Portal = PortalConfig{list("portal.relays"), flag("portal.discovery"), num("portal.max_active_relays"), flag("portal.hide")}
	c.Zrok = ZrokConfig{str("zrok.environment"), str("zrok.namespace")}
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
