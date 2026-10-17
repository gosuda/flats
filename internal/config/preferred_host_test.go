package config

import (
	"strings"
	"testing"
)

// console.preferred_host names the loopback address or a listed console
// domain, never a free URL.
func TestPreferredHostSyntax(t *testing.T) {
	d, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"network.permitted", "tailscale.console_hosts", "netbird.console_hosts"} {
		v := map[string]string{"network.permitted": "tailscale,netbird", "tailscale.console_hosts": "flats,admin", "netbird.console_hosts": "ops"}[k]
		if err := d.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"local", "tailscale:flats", "tailscale:admin", "netbird:ops", ""} {
		if err := d.Set(PreferredHostKey, v); err != nil {
			t.Errorf("Set(%q): %v", v, err)
		}
	}
	for _, v := range []string{"https://x", "foo", "tailscale:", "netbird:", "evil.example", "tailscale:Flats",
		"tailscale:a.b", ":flats", "Local", "local:", "zrok:flats", "tailscale:flats:1", " local"} {
		err := d.Set(PreferredHostKey, v)
		if err == nil || errPath(err) != PreferredHostKey {
			t.Errorf("Set(%q) = %v, want a %s error", v, err, PreferredHostKey)
		}
	}
	// Well formed, but not listed in its network.
	for _, v := range []string{"tailscale:ops", "netbird:flats", "tailscale:other"} {
		err := d.Set(PreferredHostKey, v)
		if err == nil || !strings.Contains(err.Error(), "is not in") {
			t.Errorf("Set(%q) = %v, want refused as unlisted", v, err)
		}
	}
	if v, _ := d.Stored(PreferredHostKey); v != "" {
		t.Errorf("a refused value changed the document: %q", v)
	}
}

// Removing the preferred name from its list unsets the preference in the
// same change; other list changes keep it.
func TestPreferredHostFollowsTheList(t *testing.T) {
	d, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	set := func(k, v string) {
		t.Helper()
		if err := d.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	pref := func() string { v, _ := d.Stored(PreferredHostKey); s, _ := v.(string); return s }
	set("network.permitted", "tailscale,netbird")
	set("tailscale.console_hosts", "flats,admin")
	set("netbird.console_hosts", "admin")
	set(PreferredHostKey, "tailscale:admin")
	set("tailscale.console_hosts", "flats,admin,new") // an addition
	set("netbird.console_hosts", "")                  // the other network's same name
	if pref() != "tailscale:admin" {
		t.Fatalf("an unrelated list change unset the preference: %q", pref())
	}
	set("tailscale.console_hosts", "flats,new")
	if _, ok := d.Stored(PreferredHostKey); ok {
		t.Fatalf("deleting the preferred domain kept the preference: %q", pref())
	}
	// Unset of the list counts as deleting every name.
	set("netbird.console_hosts", "ops")
	set(PreferredHostKey, "netbird:ops")
	if err := d.Unset("netbird.console_hosts"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Stored(PreferredHostKey); ok {
		t.Fatal("unsetting the list kept the preference")
	}
	// local never depends on a list.
	set(PreferredHostKey, "local")
	set("tailscale.console_hosts", "")
	if pref() != "local" {
		t.Fatalf("local preference = %q", pref())
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A file that names an unlisted domain still loads: the preference is
// ignored with an explanation, never refused.
func TestPreferredHostFileEdit(t *testing.T) {
	l, err := Parse([]byte(fileWith("network", `"permitted": ["tailscale"]`, "tailscale", `"console_hosts": ["flats"]`,
		"console", `"preferred_host": "tailscale:gone"`)))
	if err != nil {
		t.Fatalf("a mismatched preference must not refuse the file: %v", err)
	}
	c, err := l.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Preferred(); ok {
		t.Fatal("an unlisted preference is in effect")
	}
	if p := c.PreferredHostProblem(); !strings.Contains(p, "gone") || !strings.Contains(p, "ignored") {
		t.Fatalf("problem = %q", p)
	}
	// The same file with the name listed for one run uses it.
	c, err = l.Doc.Effective(map[string]string{"tailscale.console_hosts": "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if n, name, ok := c.Preferred(); !ok || n != "tailscale" || name != "gone" || c.PreferredHostProblem() != "" {
		t.Fatalf("Preferred = %q %q %v", n, name, ok)
	}
	// A malformed value is still refused as any bad value is.
	if _, err := Parse([]byte(fileWith("console", `"preferred_host": "https://evil.example"`))); errPath(err) != PreferredHostKey {
		t.Fatalf("malformed preference: %v", err)
	}
	// Unset and local have no problem; encoding keeps the console section.
	for _, v := range []string{"", "local"} {
		d, _ := New(testID, "/var/lib/flats")
		if err := d.Set(PreferredHostKey, v); err != nil {
			t.Fatal(err)
		}
		c, _ := d.Effective(nil)
		if c.PreferredHostProblem() != "" {
			t.Errorf("%q: problem %q", v, c.PreferredHostProblem())
		}
		if !strings.Contains(string(d.Encode()), `"console": {`) {
			t.Errorf("%q: encoded without the console section:\n%s", v, d.Encode())
		}
		if _, err := Parse(d.Encode()); err != nil {
			t.Errorf("%q: the encoded file does not parse: %v", v, err)
		}
	}
}
