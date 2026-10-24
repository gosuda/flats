package api

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

func TestConsoleActorArrival(t *testing.T) {
	saved := hostAddrs
	t.Cleanup(func() {
		hostAddrs = saved
		hostAddrCache.read = time.Time{}
	})
	hostAddrs = func() []netip.Addr {
		return []netip.Addr{
			netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.168.1.5"),
			netip.MustParseAddr("2001:db8::5"), netip.MustParseAddr("fe80::5%en0"), netip.MustParseAddr("169.254.1.5"),
		}
	}
	hostAddrCache.read = time.Time{}

	const loopback = "the console on the loopback listener (no tailnet identity)"
	for _, tc := range []struct {
		name, remote string
		ctx          func(context.Context) context.Context
		want         string
	}{
		{"ipv4 loopback", "127.0.0.1:5000", nil, loopback},
		{"ipv6 loopback", "[::1]:5000", nil, loopback},
		{"mapped loopback", "[::ffff:127.0.0.1]:5000", nil, loopback},
		{"host lan address", "192.168.1.5:5000", nil, "the console on this host from 192.168.1.5 (no tailnet identity)"},
		{"mapped host address", "[::ffff:192.168.1.5]:5000", nil, "the console on this host from 192.168.1.5 (no tailnet identity)"},
		{"host global ipv6", "[2001:db8::5]:5000", nil, "the console on this host from 2001:db8::5 (no tailnet identity)"},
		{"host link-local", "[fe80::5%en0]:5000", nil, "the console on this host from fe80::5%en0 (no tailnet identity)"},
		{"same link-local on another link", "[fe80::5%en1]:5000", nil, "the console listener from fe80::5%en1 (no authenticated identity)"},
		{"other link-local on the host's link", "[fe80::20%en0]:5000", nil, "the console listener from fe80::20%en0 (no authenticated identity)"},
		{"ipv4 link-local", "169.254.1.5:5000", nil, "the console listener from 169.254.1.5 (no authenticated identity)"},
		{"other machine", "192.168.1.20:5000", nil, "the console listener from 192.168.1.20 (no authenticated identity)"},
		{"mapped other machine", "[::ffff:192.168.1.20]:5000", nil, "the console listener from 192.168.1.20 (no authenticated identity)"},
		{"other machine ipv6", "[2001:db8::20]:5000", nil, "the console listener from 2001:db8::20 (no authenticated identity)"},
		{"unparsable source", "somewhere", nil, "the console listener from an unknown address (no authenticated identity)"},
		{"external origin", "127.0.0.1:5000", func(c context.Context) context.Context {
			return context.WithValue(c, externalOriginKey{}, "https://flats.example.org")
		}, "the console via external origin https://flats.example.org (no authenticated identity)"},
		{"tailnet approver", "192.0.2.1:5000", func(c context.Context) context.Context {
			return context.WithValue(c, approverKey{}, "alice@example.com")
		}, `tailnet user "alice@example.com"`},
		{"netbird approver", "192.0.2.1:5000", func(c context.Context) context.Context {
			return context.WithValue(c, approverKey{}, "netbird:laptop.netbird.cloud")
		}, `NetBird peer "laptop.netbird.cloud"`},
		{"tailscale domain login", "192.0.2.1:5000", func(c context.Context) context.Context {
			return core.WithTailnetPeer(core.WithConsoleDomain(c, "tailscale", "admin"), "bob@example.com", "laptop")
		}, `tailnet user "bob@example.com"`},
		{"tailscale domain tagged node", "192.0.2.1:5000", func(c context.Context) context.Context {
			return core.WithTailnetPeer(core.WithConsoleDomain(c, "tailscale", "admin"), "", "ci-runner")
		}, `tailnet node "ci-runner"`},
		{"tailscale domain without peer", "192.0.2.1:5000", func(c context.Context) context.Context {
			return core.WithConsoleDomain(c, "tailscale", "admin")
		}, "the console on a Tailscale console domain (no tailnet identity)"},
		{"netbird domain peer", "192.0.2.1:5000", func(c context.Context) context.Context {
			return core.WithNetbirdPeer(core.WithConsoleDomain(c, "netbird", "admin"), "ci.netbird.cloud")
		}, `NetBird peer "ci.netbird.cloud"`},
		{"netbird domain without peer", "192.0.2.1:5000", func(c context.Context) context.Context {
			return core.WithConsoleDomain(c, "netbird", "admin")
		}, "the console on a NetBird console domain (unknown peer)"},
	} {
		r := httptest.NewRequest("PUT", "http://console.example/console/api/experiments/autopilot", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("X-Real-Ip", "127.0.0.1")
		r.Header.Set("Tailscale-User-Login", "forged@example.com")
		if tc.ctx != nil {
			r = r.WithContext(tc.ctx(r.Context()))
		}
		if got := consoleActor(r); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A host address added after the cache was read is found on the next miss.
func TestHostAddrRereadOnMiss(t *testing.T) {
	saved := hostAddrs
	t.Cleanup(func() {
		hostAddrs = saved
		hostAddrCache.read = time.Time{}
	})
	addrs := []netip.Addr{netip.MustParseAddr("192.168.1.5")}
	hostAddrs = func() []netip.Addr { return addrs }
	hostAddrCache.read = time.Time{}
	added := netip.MustParseAddr("10.0.0.7")
	if IsHostAddr(added) {
		t.Fatal("unknown address matched")
	}
	addrs = append(addrs, added)
	if !IsHostAddr(added) {
		t.Fatal("address added after the cache was read is not found")
	}
}
