package board

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// Anonymous public editing (docs/spec/board-public-edit-TECH.md): guests,
// viewers' numbers, the access level and its epoch, the guest limits and
// the safe point.

// PublicAccessHeader carries the flat's public access level ("view" or
// "edit") on Public requests. Core sets it; clients cannot.
const PublicAccessHeader = "X-Flats-Public-Access"

// Public access levels.
const (
	AccessNone = ""
	AccessView = "view"
	AccessEdit = "edit"
)

// KindSafePoint is the outbox and revision kind of a safe point.
const KindSafePoint = "safe_point"

// Fatal codes of public editing.
const (
	FatalEditingEnded = "editing_ended"
	FatalFull         = "full"
	FatalNameInvalid  = "name_invalid"
)

// Reject codes of guest batches (TECH public edit 3.5, 4).
const (
	CodeGuestRate   = "guest_rate"
	CodeGuestSize   = "guest_size"
	CodeGuestRoom   = "guest_room"
	CodePermission  = "permission"
	CodeGuestAsset  = engine.CodeGuestAsset
	CodeGuestRevive = engine.CodeGuestRevive
)

// publicLimits are the guest limits of one board (TECH public edit 3.1, 4).
// The buckets use the wall clock; the numbers are fields so tests can
// lower them.
type publicLimits struct {
	guestSockets  int     // simultaneous guest sockets
	guestSessions int     // guest session rows kept
	guestWelcomes *bucket // new guest welcomes, 30 a minute
	guestOps      *bucket // operations of all guests, 200 a second
	guestBytes    *bucket // bytes read from guest sockets, 2 MiB a second
	guestPres     *bucket // presence frames relayed from guests, 200 a second

	frameMax     int   // bytes of one guest frame
	opsMax       int   // operations of one guest batch
	chargeMax    int64 // the guests' byte budget (meta.guest_charge)
	createdMax   int   // created rows of one guest session
	createdTotal int   // created rows of every guest session
	groupsMax    int64 // group ids first used by guests (meta.guest_groups)
	receiptsMax  int64 // guests' receipts
	undoMax      int64 // guests' undo stacks
	deletedMax   int64 // rows guests deleted
	roomShare    int   // percent of the disk quota, elements and assets guests may fill
	presRate     float64
}

func newPublicLimits() publicLimits {
	return publicLimits{
		guestSockets: 50, guestSessions: 200,
		guestWelcomes: newBucket(0.5, 30), guestOps: newBucket(200, 200),
		guestBytes: newBucket(2<<20, 2<<20), guestPres: newBucket(200, 200),
		frameMax: 64 << 10, opsMax: 100, chargeMax: 512 << 20,
		createdMax: 5000, createdTotal: 50000, groupsMax: 2000,
		receiptsMax: 4 << 20, undoMax: 8 << 20, deletedMax: 4 << 20,
		roomShare: 90, presRate: 10,
	}
}

// bucket is a token bucket shared by goroutines, on the wall clock.
type bucket struct {
	mu          sync.Mutex
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newBucket(perSecond, burst float64) *bucket {
	return &bucket{rate: perSecond, burst: burst, tokens: burst}
}

func (b *bucket) refill() {
	now := time.Now()
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
}

// take takes n tokens when the bucket holds them.
func (b *bucket) take(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// count takes n tokens whether or not the bucket holds them, down to one
// burst of debt, and reports whether it held them: refused traffic counts
// too, so a flood stays refused.
func (b *bucket) count(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	ok := b.tokens >= n
	b.tokens = max(b.tokens-n, -b.burst)
	return ok
}

func validLevel(level string) bool {
	return level == AccessNone || level == AccessView || level == AccessEdit
}

// levelNowValue is the level for readers outside the apply loop.
func (in *Instance) levelNowValue() string {
	l, _ := in.levelNow.Load().(string)
	return l
}

// guestCapable reports whether a Public request may become a guest: both
// the header core set from the stored level and the instance's own level
// say edit, so a race between the two fails closed (TECH public edit 2.6).
func guestCapable(r *http.Request, in *Instance) bool {
	return !private(r) && r.Header.Get(PublicAccessHeader) == AccessEdit && in.levelNowValue() == AccessEdit
}

// validToken reports whether s has the form of a token the server issues.
func validToken(s string) bool {
	if len(s) != 32 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') })
}

// guestKey is the editor key of a guest token (TECH public edit 3.1).
func guestKey(token string) string { return model.GuestKeyPrefix + sha256Hex([]byte(token))[:16] }

// guestWelcome admits a guest: its name, its server-issued key, the guest
// caps, then a session and a fresh guest snapshot (TECH public edit 3).
func (in *Instance) guestWelcome(r *http.Request, c *conn, h helloMsg) error {
	name, ok := model.CleanGuestName(h.Guest.Name)
	if !ok {
		c.final(map[string]string{"t": "fatal", "code": FatalNameInvalid,
			"detail": "a name is 1 to " + strconv.Itoa(model.MaxGuestName) + " characters, without control or invisible characters"})
		return nil
	}
	if !in.pub.guestWelcomes.take(1) || !in.hub.admitGuest(c, in.pub.guestSockets) {
		c.fatal(FatalFull)
		return nil
	}
	c.mu.Lock()
	c.guest, c.chargeGuest = true, true
	c.mu.Unlock()
	c.name, c.label = name, name
	c.batches = rate{perSec: 10, burst: 20}
	c.pres = rate{perSec: in.pub.presRate, burst: in.pub.presRate}
	if !in.publicStarts.wait(r.Context(), publicWait) {
		c.close(1013, "busy; try again")
		return nil
	}
	return in.do(r.Context(), func() error {
		if in.level != AccessEdit {
			c.fatal(FatalEditingEnded)
			return nil
		}
		st := in.st
		now := in.now().Unix()
		// A token selects an identity only while a live guest session of
		// its key exists, so it was issued here and has not expired; any
		// other token gets a fresh one (TECH public edit 3.1).
		token, issued := h.Guest.Token, ""
		if !validToken(token) || !st.LiveSession(guestKey(token), now) {
			token = randomToken()
			issued = token
		}
		c.key = guestKey(token)
		c.color = colorOf(c.key)
		t, err := in.begin()
		if err != nil {
			c.close(1011, "unavailable")
			return nil
		}
		res := st.Hello(engine.Hello{Session: h.Session, Pending: h.Pending.Count, NewID: randomToken(),
			Key: c.key, Label: c.label, Now: now})
		changes := []engine.Changed{res.Changed}
		if res.New {
			changes = append(changes, st.CapGuestSessions(in.pub.guestSessions, in.openSessions(res.Session)))
		}
		for _, ch := range changes {
			if err := t.persist(ch, st.Seq()); err != nil {
				t.rollback()
				c.close(1011, "unavailable")
				return err
			}
		}
		if err := t.commit(); err != nil {
			c.close(1011, "unavailable")
			return err
		}
		if res.Fatal != "" {
			c.fatal(res.Fatal)
			return nil
		}
		c.session, c.epoch = res.Session, in.epoch
		in.hub.join(c)
		welcome := map[string]any{"t": "welcome", "session": res.Session, "expires": now + engine.GuestSessionTTL, "access": "guest",
			"level": in.level, "me": map[string]any{"key": c.plabel, "label": c.name, "guest": true, "color": c.color}}
		if issued != "" {
			welcome["guest_token"] = issued
		}
		c.send(welcome)
		// No history for guests: every welcome, a reconnect included, gets
		// a fresh snapshot of the live elements (TECH public edit 3.2).
		c.queueSnapshot(false)
		in.sendPresence(c)
		in.hub.register(c)
		return nil
	})
}

// viewerWelcome admits a viewer with its number (spec rule 9).
func (in *Instance) viewerWelcome(r *http.Request, c *conn, h helloMsg) error {
	if !in.publicStarts.wait(r.Context(), publicWait) {
		c.close(1013, "busy; try again")
		return nil
	}
	return in.do(r.Context(), func() error {
		in.hub.viewerNumber(c, h.ViewerToken)
		c.send(map[string]any{"t": "welcome", "session": "", "expires": 0, "access": "public", "level": in.level,
			"me": map[string]any{"label": "Viewer " + strconv.Itoa(c.viewerN), "viewer": c.viewerN}, "viewer_token": c.viewerToken})
		c.queueSnapshot(false)
		in.hub.register(c)
		in.notifyViewers()
		return nil
	})
}

// openSessions are the sessions of the open sockets, with extra.
func (in *Instance) openSessions(extra ...string) map[string]bool {
	open := map[string]bool{}
	for _, c := range in.hub.list() {
		if c.session != "" {
			open[c.session] = true
		}
	}
	for _, s := range extra {
		open[s] = true
	}
	return open
}

// onGuestOps reads a guest's batch under the guest limits of the reader
// (TECH public edit 4.1, 4.2): within tells whether its bytes fitted the
// board's guest byte budget, which readLoop charged for every frame. A
// frame over the budget or over the size limit is refused from its batch
// number alone, without decoding its operations; the socket stays open.
func (in *Instance) onGuestOps(ctx context.Context, c *conn, raw string, within bool) bool {
	var head struct {
		N int64 `json:"n"`
	}
	if json.Unmarshal([]byte(raw), &head) != nil || head.N <= 0 {
		return false
	}
	b := &batchReq{c: c, n: head.N}
	switch {
	case !within:
		b.transient(CodeGuestRate)
		return true
	case len(raw) > in.pub.frameMax:
		b.refuse = &engine.Reject{Code: CodeGuestSize, Detail: "a guest's change is at most " + strconv.Itoa(in.pub.frameMax>>10) + " KiB"}
		return in.submit(ctx, b)
	}
	var m struct {
		Ops json.RawMessage `json:"ops"`
	}
	var ops []json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil {
		return false
	}
	if json.Unmarshal(m.Ops, &ops) == nil && len(ops) > in.pub.opsMax {
		b.refuse = &engine.Reject{Code: CodeGuestSize, Detail: "a guest's change holds at most " + strconv.Itoa(in.pub.opsMax) + " operations"}
	} else {
		b.raw = m.Ops
	}
	return in.submit(ctx, b)
}

// guestPres relays a guest's presence within the board's guest byte and
// presence budgets.
func (in *Instance) guestPres(c *conn, raw string, within bool) bool {
	if !within || len(raw) > in.pub.frameMax || !in.pub.guestPres.take(1) {
		return true
	}
	return in.onPres(c, raw)
}

// guestRoom is what the room checks of a guest batch compare with: the
// board before the batch (TECH public edit 4.3, 4.4).
type guestRoom struct {
	quota        int64
	usage        int64 // disk usage before the batch; 0 without a quota
	alive        int
	assets       int
	created      int
	createdTotal int
	session      string
}

// measureRoom takes the board's state before a guest batch.
func (in *Instance) measureRoom(t *txn, c *conn, quota int64) (*guestRoom, error) {
	st := in.st
	r := &guestRoom{quota: quota, session: c.session, created: st.CreatedCount(c.session), createdTotal: st.GuestCreated()}
	r.alive, r.assets = st.Counts()
	if quota > 0 {
		other, ok := in.meas.get()
		if ok {
			u, err := t.usage(other)
			if err != nil {
				return nil, err
			}
			r.usage = u
		}
	}
	return r, nil
}

// checkRoom checks a guest batch after it applied and before it commits
// (TECH public edit 4.3, 4.4): the reserved disk room, the guests' byte
// budget, the reserved element and asset room, and the guests' created
// rows and group ids. It returns why the batch is refused, or "", and
// charges the budget and the group ids it used.
func (in *Instance) checkRoom(t *txn, r *guestRoom, c engine.Changed) (string, error) {
	st, lim := in.st, in.pub
	alive, assets := st.Counts()
	l := st.Limits()
	if alive > r.alive && alive > l.Elements*lim.roomShare/100 {
		return "the board is nearly full; the remaining room is kept for its owners", nil
	}
	if assets > r.assets && assets > l.Assets*lim.roomShare/100 {
		return "the board holds nearly all the images it can; the remaining room is kept for its owners", nil
	}
	if n := st.CreatedCount(r.session); n > r.created && n > lim.createdMax {
		return "you have created too many elements in this session", nil
	}
	if n := st.GuestCreated(); n > r.createdTotal && n > lim.createdTotal {
		return "guests have created too many elements on this board", nil
	}
	groups := int64(0)
	for _, k := range c.Created {
		if k.Session == "" {
			if _, ok := st.CreatedInc(k); ok {
				groups++
			}
		}
	}
	if groups > 0 {
		if t.guestGroups+groups > lim.groupsMax {
			return "guests have created too many groups on this board", nil
		}
		t.guestGroups += groups
		if err := setMeta(t.ctx, t.tx, "guest_groups", t.guestGroups); err != nil {
			return "", err
		}
	}
	if r.quota <= 0 || r.usage == 0 {
		return "", nil
	}
	other, ok := in.meas.get()
	if !ok {
		return "", nil
	}
	u, err := t.usage(other)
	if err != nil {
		return "", err
	}
	if u > r.quota/100*int64(lim.roomShare) {
		return "the board is nearly out of disk space; the remaining room is kept for its owners", nil
	}
	if grew := u - r.usage; grew > 0 {
		if t.guestCharge+grew > lim.chargeMax {
			return "guests have used all the space this board gives them", nil
		}
		t.guestCharge += grew
		if err := setMeta(t.ctx, t.tx, "guest_charge", t.guestCharge); err != nil {
			return "", err
		}
	}
	return "", nil
}

// refundGuestCharge lowers the guests' byte budget charge by the fall of
// the board's measured usage since the previous tick (TECH public edit
// 4.3): freeing space, by retention or deletion, returns it to guests.
func (in *Instance) refundGuestCharge(t *txn) error {
	quota := in.b.Limits().DiskQuota
	other, ok := in.meas.get()
	if quota <= 0 || !ok {
		return nil
	}
	u, err := t.usage(other)
	if err != nil {
		return err
	}
	last := in.sd.lastUsage
	t.after = append(t.after, func() { in.sd.lastUsage = u })
	if last == 0 || u >= last || t.guestCharge == 0 {
		return nil
	}
	t.guestCharge = max(0, t.guestCharge-(last-u))
	return setMeta(t.ctx, t.tx, "guest_charge", t.guestCharge)
}

// trimGuestState applies the guest caps of the server state in the tick
// (TECH public edit 3.2, 4.4).
func (in *Instance) trimGuestState(now int64) []engine.Changed {
	st := in.st
	return []engine.Changed{
		st.CapGuestSessions(in.pub.guestSessions, in.openSessions()),
		st.TrimReceiptsCapped(16<<20, in.pub.receiptsMax, now),
		st.TrimDeletedCapped(16<<20, in.pub.deletedMax),
	}
}

// SetPublicAccess changes the instance's public access level (TECH 6.1).
// It returns after the apply loop has bumped the access epoch: no guest
// batch of the earlier epoch commits after it. Leaving edit closes the
// guests' edit group and every guest socket with editing_ended.
func (in *Instance) SetPublicAccess(ctx context.Context, level string) error {
	if !validLevel(level) {
		return errorf(CategoryInvalid, "unknown public access level %q", level)
	}
	return in.do(ctx, func() error {
		old := in.level
		in.level = level
		in.levelNow.Store(level)
		in.epoch++
		if level != AccessEdit {
			if err := in.closeGuestGroup(); err != nil {
				in.log.Warn("board guest group", "err", err)
			}
		}
		for _, c := range in.hub.list() {
			switch {
			case c.guest:
				c.fatal(FatalEditingEnded)
			case old != level:
				c.send(map[string]any{"t": "level", "access": level})
			}
		}
		return nil
	})
}

// closeGuestGroup records a person entry when the guests' group is dirty
// and deletes it (leaving edit, TECH public edit 5.1).
func (in *Instance) closeGuestGroup() error {
	if _, ok := in.sd.groups[guestGroup]; !ok {
		return nil
	}
	t, err := in.begin()
	if err != nil {
		return err
	}
	if t.groups[guestGroup].Dirty {
		if _, err := t.record(recordSpec{kind: "person", authors: t.dirtyAuthors()}); err != nil {
			t.rollback()
			return err
		}
	}
	if err := t.deleteGroup(guestGroup); err != nil {
		t.rollback()
		return err
	}
	return t.commit()
}

// SafePoint closes open groups and records a forced safe point entry unless
// the board's file list equals prev (TECH 6.2). It returns the entry's outbox
// seq, or reuse when nothing was recorded because the content equals prev.
func (in *Instance) SafePoint(ctx context.Context, prev []File) (seq int64, reuse bool, err error) {
	err = in.do(ctx, func() error {
		t, err := in.begin()
		if err != nil {
			return err
		}
		fail := func(err error) error { t.rollback(); return err }
		if t.anyDirtyGroup() {
			if _, err := t.record(recordSpec{kind: "person", authors: t.dirtyAuthors()}); err != nil {
				return fail(err)
			}
		}
		if err := t.deleteGroups(); err != nil {
			return fail(err)
		}
		files, _ := t.files(false)
		reuse = prev != nil && equalFiles(files, prev)
		if !reuse {
			if seq, err = t.record(recordSpec{kind: KindSafePoint, forced: true, authors: []string{},
				message: "Safe point before guest editing"}); err != nil {
				return fail(err)
			}
		}
		return t.commit()
	})
	return seq, reuse, err
}

// PublicPresence reports how many guests and viewers are connected now.
// It does not block.
func (in *Instance) PublicPresence() (guests, viewers int) {
	return int(in.hub.guestsNow.Load()), int(in.hub.viewersNow.Load())
}

// viewerFrames sends the viewers frame to members and guests: at most once
// a second, and only when the viewers changed (TECH public edit 3.4).
type viewerFrames struct {
	mu      sync.Mutex
	last    []byte
	at      time.Time
	pending bool
}

// viewersListed is how many viewer numbers the frame lists; above it the
// frame gives only the count (spec rule 10).
const viewersListed = 20

// frame encodes the viewers frame of numbers, or nil when no viewer is on
// the board.
func (v *viewerFrames) frame(numbers []int) []byte {
	if len(numbers) == 0 {
		return nil
	}
	var b []byte
	if len(numbers) > viewersListed {
		b, _ = json.Marshal(map[string]any{"t": "viewers", "count": len(numbers)})
	} else {
		b, _ = json.Marshal(map[string]any{"t": "viewers", "n": numbers})
	}
	return b
}

// notifyViewers schedules a viewers frame after a viewer joined or left.
func (in *Instance) notifyViewers() {
	v := &in.viewers
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pending {
		return
	}
	v.pending = true
	time.AfterFunc(max(0, time.Second-time.Since(v.at)), in.flushViewers)
}

func (in *Instance) flushViewers() {
	if in.stopped.Load() {
		return
	}
	v := &in.viewers
	numbers := in.hub.viewerList()
	f := v.frame(numbers)
	if f == nil {
		f, _ = json.Marshal(map[string]any{"t": "viewers", "n": []int{}})
	}
	v.mu.Lock()
	v.pending = false
	same := string(f) == string(v.last)
	if !same {
		v.last, v.at = f, time.Now()
	}
	v.mu.Unlock()
	if same {
		return
	}
	for _, c := range in.hub.list() {
		if !c.public || c.guest {
			c.enqueue(f)
		}
	}
}

// ContentEquals reports, without recording anything, whether the live
// board's current file list (including edits not yet recorded) equals files.
// Core uses it to decide safe point reuse before checking for a free pin.
func (in *Instance) ContentEquals(ctx context.Context, files []File) (bool, error) {
	var equal bool
	err := in.do(ctx, func() error {
		sd := in.sd
		t := &txn{in: in, newest: sd.newest, dirty: sd.dirty, bucketBytes: sd.bucketBytes}
		cur, _ := t.files(false)
		equal = equalFiles(cur, files)
		return nil
	})
	return equal, err
}

// equalFiles reports whether a file list, sorted by path as files returns
// it, equals files in any order.
func equalFiles(cur, files []File) bool {
	want := slices.Clone(files)
	slices.SortFunc(want, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return slices.Equal(cur, want)
}
