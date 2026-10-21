package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTemplate(t *testing.T) {
	long := strings.Repeat("a", 61) + "." + strings.Repeat("b", 61) + "." + strings.Repeat("c", 61) + ".ddd" // 189 characters
	if len(long) != 189 {
		t.Fatalf("fixture is %d characters", len(long))
	}
	for _, tc := range []struct{ in, url, authority, scheme string }{
		{"https://{host}.site.example.org", "https://blog.site.example.org", "blog.site.example.org", "https"},
		{"http://{host}.flats.internal", "http://blog.flats.internal", "blog.flats.internal", "http"},
		{"https://{host}.site.example.org:8443", "https://blog.site.example.org:8443", "blog.site.example.org:8443", "https"},
		{"http://{host}.example.org:443", "http://blog.example.org:443", "blog.example.org:443", "http"},
		{"https://{host}.example.org:80", "https://blog.example.org:80", "blog.example.org:80", "https"},
		{"https://{host}.org", "https://blog.org", "blog.org", "https"},
		{"https://{host}.xn--bcher-kva.example", "https://blog.xn--bcher-kva.example", "blog.xn--bcher-kva.example", "https"},
		{"https://{host}." + long, "https://blog." + long, "blog." + long, "https"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			tmpl, err := ParseTemplate(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := tmpl.URL("blog"); got != tc.url {
				t.Errorf("URL = %q, want %q", got, tc.url)
			}
			if got := tmpl.Authority("blog"); got != tc.authority {
				t.Errorf("Authority = %q, want %q", got, tc.authority)
			}
			if got := tmpl.Scheme(); got != tc.scheme {
				t.Errorf("Scheme = %q, want %q", got, tc.scheme)
			}
			if got := tmpl.String(); got != tc.in {
				t.Errorf("String = %q, want %q", got, tc.in)
			}
		})
	}
	for _, in := range []string{
		"", "{host}.example.org", "//{host}.example.org", "ftp://{host}.example.org", "HTTPS://{host}.example.org",
		"https://example.org", "https://{host}", "https://{host}.", "https://{host}-x.example.org",
		"https://a.{host}.example.org", "https://{host}.{host}.example.org", "https://{HOST}.example.org",
		"https://{host}.example.org:443", "http://{host}.example.org:80",
		"https://{host}.example.org/", "https://{host}.example.org/path", "https://{host}.example.org?q",
		"https://{host}.example.org?", "https://{host}.example.org#f", "https://user@{host}.example.org",
		"https://{host}.example.org@evil.org", "https://{host}.127.0.0.1", "https://{host}.0x7f000001",
		"https://{host}.localhost", "https://{host}.app.localhost", "https://{host}.Example.org",
		"https://{host}.example.org.", "https://{host}.*.example.org", "https://{host}.bad_host.org",
		"https://{host}.example.org:", "https://{host}.example.org:0", "https://{host}.example.org:65536",
		"https://{host}.example.org:0443", "https://{host}.[example.org]", " https://{host}.example.org",
		"https://{host}." + long + "d",
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseTemplate(in); err == nil {
				t.Fatalf("accepted %q", in)
			} else if strings.Contains(err.Error(), templateLabel) {
				t.Errorf("error names the placeholder: %v", err)
			}
		})
	}
}

func TestTemplateLabel(t *testing.T) {
	tmpl, err := ParseTemplate("https://{host}.site.example.org:8443")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, label string
		ok          bool
	}{
		{"blog.site.example.org", "blog", true},
		{"blog.site.example.org:8443", "blog", true},
		{"blog.site.example.org:9999", "blog", true},
		{"BLOG.Site.Example.ORG", "blog", true},
		{"blog.site.example.org.", "blog", true},
		{"blog.site.example.org.:8443", "blog", true},
		{"blog-ab12cd34.site.example.org", "blog-ab12cd34", true},
		{"7th.site.example.org", "7th", true},
		{strings.Repeat("a", 63) + ".site.example.org", strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64) + ".site.example.org", "", false},
		{"a.blog.site.example.org", "", false},
		{".site.example.org", "", false},
		{"site.example.org", "", false},
		{"blog.example.org", "", false},
		{"blog.site.example.org.evil", "", false},
		{"blogsite.example.org", "", false},
		{"blog.other-site.example.org", "", false},
		{"-blog.site.example.org", "", false},
		{"blog-.site.example.org", "", false},
		{"bl--og.site.example.org", "", false},
		{"bl_og.site.example.org", "", false},
		{"", "", false},
		{"[::1]:8443", "", false},
		{"127.0.0.1:7880", "", false},
	} {
		label, ok := tmpl.Label(tc.host)
		if label != tc.label || ok != tc.ok {
			t.Errorf("Label(%q) = %q, %v; want %q, %v", tc.host, label, ok, tc.label, tc.ok)
		}
	}
	var zero Template
	if label, ok := zero.Label("blog.site.example.org"); ok || label != "" {
		t.Errorf("zero Template matched %q", label)
	}
	if zero.URL("blog") != "" || zero.Authority("blog") != "" || zero.Scheme() != "" || zero.String() != "" {
		t.Error("zero Template built an address")
	}
}

func TestProxyKeys(t *testing.T) {
	d, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy != (ProxyConfig{Addr: "127.0.0.1:7880"}) {
		t.Fatalf("default Proxy = %+v", c.Proxy)
	}
	if d.Set(ProxyTemplateKey, "https://{host}.example.org:443") == nil {
		t.Fatal("Set accepted a default port")
	}
	if d.Set("proxy.addr", "127.0.0.1") == nil {
		t.Fatal("Set accepted an address without a port")
	}
	for _, kv := range [][2]string{
		{ProxyTemplateKey, "https://{host}.site.example.org"},
		{"proxy.addr", "0.0.0.0:9000"},
		{"network.permitted", "proxy"},
		{"network.default_private", "proxy"},
	} {
		if err := d.Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := Parse(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.Encode()), "\"proxy\": {\n    \"url_template\": \"https://{host}.site.example.org\",\n    \"addr\": \"0.0.0.0:9000\"\n  }") {
		t.Errorf("encoded:\n%s", d.Encode())
	}
	c, err = loaded.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := ParseTemplate("https://{host}.site.example.org")
	if c.Proxy != (ProxyConfig{Template: want, Addr: "0.0.0.0:9000"}) {
		t.Errorf("Proxy = %+v", c.Proxy)
	}
	if got := c.Network.NewFlatProviders(); len(got) != 1 || got[0] != "proxy" {
		t.Errorf("NewFlatProviders = %v", got)
	}
	if _, err := loaded.Doc.Effective(map[string]string{"proxy.addr": "127.0.0.1:1"}); err == nil {
		t.Error("proxy.addr was overridden for one run")
	}
	for _, kv := range [][2]string{
		{"network.default_private", "proxy-public"},
		{"network.default_public", "proxy"},
		{"network.permitted", "local,proxy"},
		{"network.permitted", "proxy,proxy"},
	} {
		if d.Set(kv[0], kv[1]) == nil {
			t.Errorf("Set(%s, %s) accepted", kv[0], kv[1])
		}
	}
}

// saveSteps applies each step as its own save, as the console and offline
// `flats config set` do, and returns the error of the first refused one.
func saveSteps(t *testing.T, steps ...func(*Document) error) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := Create(path, doc)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWriter(path)
	for _, step := range steps {
		if hash, err = w.Save(hash, step); err != nil {
			return err
		}
	}
	return nil
}

func sets(kvs ...string) func(*Document) error {
	return func(d *Document) error {
		for i := 0; i < len(kvs); i += 2 {
			if err := d.Set(kvs[i], kvs[i+1]); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestProxyCrossKeyChecks(t *testing.T) {
	const tmpl = "https://{host}.site.example.org"
	unset := func(key string) func(*Document) error { return func(d *Document) error { return d.Unset(key) } }
	for _, tc := range []struct {
		name  string
		steps []func(*Document) error
		path  string // the refused field; "" when every save succeeds
	}{
		{"private kind with template", []func(*Document) error{sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy")}, ""},
		{"public kind with template", []func(*Document) error{sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy-public")}, ""},
		{"template without a kind", []func(*Document) error{sets(ProxyTemplateKey, tmpl)}, ""},
		{"both kinds", []func(*Document) error{sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy,proxy-public")}, "network.permitted"},
		{"second kind turned on", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy"), sets("network.permitted", "proxy,proxy-public"),
		}, "network.permitted"},
		{"private kind without template", []func(*Document) error{sets("network.permitted", "proxy")}, ProxyTemplateKey},
		{"public kind without template", []func(*Document) error{sets("network.permitted", "proxy-public")}, ProxyTemplateKey},
		{"template cleared while on", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy"), sets(ProxyTemplateKey, ""),
		}, ProxyTemplateKey},
		{"template unset while on", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy-public"), unset(ProxyTemplateKey),
		}, ProxyTemplateKey},
		{"template cleared after turning off", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy"), sets("network.permitted", ""), sets(ProxyTemplateKey, ""),
		}, ""},
		{"private default permitted", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy", "network.default_private", "proxy"),
		}, ""},
		{"private default not permitted", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.default_private", "proxy"),
		}, "network.default_private"},
		{"public default permitted", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy-public", "network.default_public", "proxy-public"),
		}, ""},
		{"public default not permitted", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.default_public", "proxy-public"),
		}, "network.default_public"},
		{"turned off while the default names it", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy", "network.default_private", "proxy"),
			sets("network.permitted", ""),
		}, "network.default_private"},
		{"address of the console", []func(*Document) error{sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy", "proxy.addr", "127.0.0.1:7878")}, "proxy.addr"},
		{"address of local", []func(*Document) error{sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy-public", "proxy.addr", "127.0.0.1:7879")}, "proxy.addr"},
		{"local moved onto the address", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy"), sets("host.local_addr", "127.0.0.1:7880"),
		}, "proxy.addr"},
		{"console moved onto the address", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy"), sets("host.console_addr", "127.0.0.1:7880"),
		}, "proxy.addr"},
		{"turned on over a taken address", []func(*Document) error{
			sets("host.local_addr", "127.0.0.1:7880", ProxyTemplateKey, tmpl), sets("network.permitted", "proxy"),
		}, "proxy.addr"},
		// Rule 11: without a proxy provider the address is never bound.
		{"local on the address without a proxy", []func(*Document) error{sets("host.local_addr", "127.0.0.1:7880")}, ""},
		{"console on the address without a proxy", []func(*Document) error{sets("host.console_addr", "127.0.0.1:7880", ProxyTemplateKey, tmpl)}, ""},
		{"ephemeral ports", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy",
				"host.console_addr", "127.0.0.1:0", "host.local_addr", "127.0.0.1:0", "proxy.addr", "127.0.0.1:0"),
		}, ""},
		{"other host same port", []func(*Document) error{sets(ProxyTemplateKey, tmpl, "network.permitted", "proxy", "proxy.addr", "0.0.0.0:7879")}, ""},
		{"origin then overlapping template", []func(*Document) error{
			sets(ExternalOriginsKey, "https://flats.site.example.org"), sets(ProxyTemplateKey, tmpl),
		}, ExternalOriginsKey + "[0]"},
		{"template then overlapping origin", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl), sets(ExternalOriginsKey, "https://example.org,http://flats.site.example.org:8080"),
		}, ExternalOriginsKey + "[1]"},
		{"origin on another level", []func(*Document) error{
			sets(ProxyTemplateKey, tmpl, ExternalOriginsKey, "https://site.example.org,https://a.b.site.example.org,https://flats.example.org"),
		}, ""},
		{"origin with a numeric label", []func(*Document) error{
			sets(ProxyTemplateKey, "http://{host}.flats.internal", ExternalOriginsKey, "http://1.flats.internal"),
		}, ExternalOriginsKey + "[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := saveSteps(t, tc.steps...)
			if tc.path == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Path != tc.path {
				t.Fatalf("err = %v, want a refusal of %s", err, tc.path)
			}
		})
	}
}
