package netbird

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	accountFile = "account.json"
	configFile  = "netbird.json"     // embed's persisted config (holds the peer private key)
	dnsFile     = "netbird-dns.json" // embed's state file
	portsFile   = "ports.json"

	portMin = 21000
	portMax = 21999
)

// Account describes the enrolled NetBird peer. It holds no secrets.
type Account struct {
	// ManagementURL is the NetBird management server ("" is the NetBird
	// default).
	ManagementURL string `json:"management_url"`
	DeviceName    string `json:"device_name"`
	// Fingerprint identifies the peer (a hash of its WireGuard public key).
	Fingerprint string `json:"fingerprint"`
}

// ErrNotConnected is returned by Serve calls while no NetBird peer is enrolled.
var ErrNotConnected = errors.New("netbird: not connected; enroll this host with a setup key first")

// LoadAccount reads the account enrolled in dir. ok is false when none is.
func LoadAccount(dir string) (a Account, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, accountFile))
	if errors.Is(err, os.ErrNotExist) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	if err := json.Unmarshal(b, &a); err != nil || a.Fingerprint == "" {
		return Account{}, false, fmt.Errorf("unreadable %s", filepath.Join(dir, accountFile))
	}
	if _, err := os.Stat(filepath.Join(dir, configFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Account{}, false, nil // account without identity: not usable
		}
		return Account{}, false, err
	}
	return a, true, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o600)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ports maps "private/<host>" and "public/<slug>" to the overlay TCP port
// reserved for it. Both roles share one pool so a Public flat's private
// route never collides with its public listener.
//
// A retired public port is quarantined under "quarantine/<port>", whose value
// is the Unix time it may be reused. NetBird's expose termination is
// best-effort (the client only logs a failed StopExpose), so the proxy may keep
// forwarding to that port until the service expires on the management side.
// Reusing the port at once could put another flat behind the old public URL.
type ports map[string]int

// portsMu guards ports.json across every Net and RetireStored.
var portsMu sync.Mutex

// RetireStored releases host's recorded ports when no Net is running, for a
// flat deleted while NetBird is off: the private port is freed and the public
// one quarantined, as Retire does. A host with no record is not an error.
func RetireStored(dir, host string) error {
	portsMu.Lock()
	defer portsMu.Unlock()
	p, err := readPorts(dir)
	if err != nil {
		return err
	}
	changed := false
	if _, ok := p[key(rolePrivate, host)]; ok {
		delete(p, key(rolePrivate, host))
		changed = true
	}
	if port, ok := p[key(rolePublic, host)]; ok {
		delete(p, key(rolePublic, host))
		p.quarantine(port, time.Now())
		changed = true
	}
	if !changed {
		return nil
	}
	return writeJSON(filepath.Join(dir, portsFile), p)
}

const quarantinePrefix = "quarantine/"

// quarantineTTL is how long a retired public port stays unavailable.
var quarantineTTL = 24 * time.Hour

// quarantinedPort parses a quarantine key.
func quarantinedPort(key string) (int, bool) {
	s, ok := strings.CutPrefix(key, quarantinePrefix)
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// quarantine keeps port out of the pool until now+quarantineTTL.
func (p ports) quarantine(port int, now time.Time) {
	p[quarantinePrefix+strconv.Itoa(port)] = int(now.Add(quarantineTTL).Unix())
}

// expire drops quarantines that ended and reports whether any did.
func (p ports) expire(now time.Time) bool {
	changed := false
	for k, until := range p {
		if _, ok := quarantinedPort(k); ok && int64(until) <= now.Unix() {
			delete(p, k)
			changed = true
		}
	}
	return changed
}

func readPorts(dir string) (ports, error) {
	b, err := os.ReadFile(filepath.Join(dir, portsFile))
	if errors.Is(err, os.ErrNotExist) {
		return ports{}, nil
	}
	if err != nil {
		return nil, err
	}
	p := ports{}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("unreadable %s", filepath.Join(dir, portsFile))
	}
	return p, nil
}

// allocate returns key's port, reserving the lowest free one if needed.
func (p ports) allocate(key string) (port int, added bool, err error) {
	if v, ok := p[key]; ok {
		return v, false, nil
	}
	p.expire(time.Now())
	used := make([]int, 0, len(p))
	for k, v := range p {
		if q, ok := quarantinedPort(k); ok {
			used = append(used, q)
			continue
		}
		used = append(used, v)
	}
	sort.Ints(used)
	next := portMin
	for _, v := range used {
		if v == next {
			next++
		}
	}
	if next > portMax {
		return 0, false, fmt.Errorf("no free overlay port in %d-%d", portMin, portMax)
	}
	p[key] = next
	return next, true, nil
}
