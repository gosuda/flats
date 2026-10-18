package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestExternalOriginsValidation(t *testing.T) {
	valid := []string{"https://flats.example.org", "http://private-host:8080", "https://xn--bcher-kva.example:443"}
	d, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(valid)
	if err := d.Set(ExternalOriginsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	loaded, err := Parse(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	c, err := loaded.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Console.ExternalOrigins, valid) {
		t.Fatalf("origins = %v", c.Console.ExternalOrigins)
	}
	for _, value := range []string{
		"https://2130706433", "https://127.1", "https://0x7f000001", "https://localhost", "https://app.localhost",
		"https://EXAMPLE.org", "HTTPS://example.org", "https://example.org/", "https://example.org/path",
		"https://example.org?", "https://example.org?x", "https://example.org#", "https://example.org#x",
		"https://user@example.org", "//example.org", "ftp://example.org", "https://*.example.org",
		"https://example.org.", "https://127.0.0.1", "https://[::1]", "https://example.org:",
		"https://example.org:0", "https://example.org:65536", "https://example.org:0443",
		"https://bad_host.org", "https://-bad.org", "https://bücher.example", "https://[example.org]",
		"https://example.org,https://example.org:443",
		"https://example.org:443,http://example.org",
		"http://example.org:80,https://example.org",
		"https://example.org,https://example.org", "https://example.org,http://example.org",
	} {
		t.Run(value, func(t *testing.T) {
			if err := d.Set(ExternalOriginsKey, value); err == nil {
				t.Fatalf("accepted %q", value)
			}
			c, _ := d.Effective(nil)
			if !reflect.DeepEqual(c.Console.ExternalOrigins, valid) {
				t.Fatal("rejected update changed document")
			}
		})
	}
	if err := d.Unset(ExternalOriginsKey); err != nil {
		t.Fatal(err)
	}
	c, _ = d.Effective(nil)
	if len(c.Console.ExternalOrigins) != 0 {
		t.Fatal("unset must restore empty default")
	}
}
