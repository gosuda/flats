package api

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// hostAddrs returns this host's interface addresses, each link-local one
// carrying its interface name as zone. Tests replace it.
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
			if ip.IsLinkLocalUnicast() {
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

// isHostAddr reports whether src, a connection's source address, is one of
// this host's own addresses: a process on the host that connects through
// its LAN or global address. A link-local address matches only on the
// interface its zone names; the same address on another link is another
// machine. The addresses are cached for 10 seconds and read again once on
// a miss, so a newly assigned address is found.
func isHostAddr(src netip.Addr) bool {
	match := func(addrs []netip.Addr) bool {
		for _, a := range addrs {
			if src.IsLinkLocalUnicast() && src == a || !src.IsLinkLocalUnicast() && src.WithZone("") == a.WithZone("") {
				return true
			}
		}
		return false
	}
	c := &hostAddrCache
	c.Lock()
	defer c.Unlock()
	fresh := time.Since(c.read) < 10*time.Second
	if fresh && match(c.addrs) {
		return true
	}
	c.addrs, c.read = hostAddrs(), time.Now()
	return match(c.addrs)
}
