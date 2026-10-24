package api

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// hostAddrs returns this host's interface addresses, each IPv6 link-local
// one carrying its interface name as zone. Tests replace it.
var hostAddrs = interfaceAddrs

func interfaceAddrs() []netip.Addr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.IsLinkLocalUnicast() && ip.Is6() {
				ip = ip.WithZone(ifc.Name)
			}
			out = append(out, ip)
		}
	}
	return out
}

var hostAddrCache struct {
	sync.Mutex
	addrs []netip.Addr
	read  time.Time
}

// IsHostAddr reports whether src, a connection's unmapped source address,
// is one of this host's own addresses: a process on the host that connects
// through its LAN or global address. The addresses are cached for 10
// seconds and read again once on a miss, so a newly assigned address is
// found.
func IsHostAddr(src netip.Addr) bool {
	c := &hostAddrCache
	c.Lock()
	defer c.Unlock()
	if time.Since(c.read) < 10*time.Second && hostAddrIn(c.addrs, src) {
		return true
	}
	c.addrs, c.read = hostAddrs(), time.Now()
	return hostAddrIn(c.addrs, src)
}

// hostAddrIn compares without zones, except for link-local addresses: those
// match only on the interface their zone names, since the same link-local
// address on another link is another machine. An IPv4 link-local address
// carries no interface, so it never matches.
func hostAddrIn(addrs []netip.Addr, src netip.Addr) bool {
	for _, a := range addrs {
		if a.IsLinkLocalUnicast() {
			if a.Zone() != "" && a == src {
				return true
			}
		} else if a.WithZone("") == src.WithZone("") {
			return true
		}
	}
	return false
}
