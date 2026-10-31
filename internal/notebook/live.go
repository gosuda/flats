package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

var livePublicBudget = func() *wsconn.Budget { b := new(wsconn.Budget); b.Lower(256 << 20); return b }()

type liveHub struct {
	mu              sync.Mutex
	conns           map[*liveConn]bool
	drainingPublic  map[*wsconn.Conn]bool
	resumes         map[string]liveResume
	starts          liveRate
	publicEpoch     uint64
	private, public int
}

type liveResume struct {
	spill          *Spill
	session, epoch string
	public         bool
}

type liveOutbound struct {
	kind  string
	value any
	ack   *Ack
	watch *liveWatch
}
type liveNotice struct {
	view ID
	seq  Sequence
}

type liveSnapshot struct {
	generation int
	data       []byte
	err        error
	delivered  chan struct{}
	run        int
	ack        *Ack
	receipt    *Ack
	database   ID
	seq        Sequence
}

type liveConn struct {
	in                 *Instance
	ws                 *wsconn.Conn
	ctx                context.Context
	cancel             context.CancelFunc
	public             bool
	epoch              string
	exposure           uint64
	baseSeq            Sequence
	hydrationRun       int
	noticeSeen         map[liveNotice]bool
	receipts           map[*Ack]context.CancelFunc
	session            Session
	wake               chan struct{}
	commands           chan liveOutbound
	snapshots          chan liveSnapshot
	generation         int
	seq                Sequence
	scopes             []Scope
	hydration          context.CancelFunc
	hydrationWG        sync.WaitGroup
	resynced           bool
	presence           livePresence
	presencePending    bool
	welcomed           bool
	actions, presences liveRate
}

func newLiveHub() *liveHub {
	return &liveHub{conns: map[*liveConn]bool{}, drainingPublic: map[*wsconn.Conn]bool{}, resumes: map[string]liveResume{}}
}

func (in *Instance) publishLive() {
	in.live.mu.Lock()
	defer in.live.mu.Unlock()
	for c := range in.live.conns {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (in *Instance) closeLive(code string) {
	in.live.mu.Lock()
	defer in.live.mu.Unlock()
	for c := range in.live.conns {
		if c.public {
			c.ws.Abort()
		} else {
			if code == "deleted" || code == "renamed" {
				c.ws.Send(string(mustLiveEnvelope("fatal", map[string]any{"code": code})))
			}
			c.ws.Close(1001, code)
		}
		c.cancel()
	}
	for ws := range in.live.drainingPublic {
		ws.Abort()
	}
	in.spills.CancelScope(func(Scope) bool { return true })
	clear(in.live.resumes)
}

// CancelPublic closes current Public streams and their disconnected snapshot handles.
// Core calls this when withdrawing a route with no active request to cancel.
func (in *Instance) CancelPublic() {
	in.live.mu.Lock()
	defer in.live.mu.Unlock()
	in.live.publicEpoch++
	for c := range in.live.conns {
		if c.public {
			c.ws.Abort()
			c.cancel()
		}
	}
	for ws := range in.live.drainingPublic {
		ws.Abort()
	}
	in.spills.CancelScope(func(s Scope) bool { return s.Public })
	for token, r := range in.live.resumes {
		if r.public {
			delete(in.live.resumes, token)
		}
	}
}

func mustLiveEnvelope(kind string, value any) []byte {
	data, _ := liveEnvelope(kind, value)
	return data
}

func (in *Instance) serveLive(w http.ResponseWriter, r *http.Request, public bool) {
	if !wsconn.IsUpgrade(r) {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	in.admit.RLock()
	if in.ctx.Err() != nil {
		in.admit.RUnlock()
		writeNotebookError(w, unavailable("stopped"))
		return
	}
	in.transports.Add(1)
	notebookConnections.Add(1)
	defer notebookConnections.Add(-1)
	in.admit.RUnlock()
	defer in.transports.Done()
	in.live.mu.Lock()
	exposure := in.live.publicEpoch
	count := in.live.private
	if public {
		count = in.live.public
	}
	limit := 64
	if public {
		limit = 256
	}
	if count >= limit {
		in.live.mu.Unlock()
		writeNotebookError(w, unavailable("connections"))
		return
	}
	if public {
		in.live.public++
	} else {
		in.live.private++
	}
	in.live.mu.Unlock()
	defer func() {
		in.live.mu.Lock()
		if public {
			in.live.public--
		} else {
			in.live.private--
		}
		in.live.mu.Unlock()
	}()
	cap := int64(livePrivateQueue)
	var shared *wsconn.Budget
	if public {
		cap = livePublicQueue
		shared = livePublicBudget
	}
	ws, err := wsconn.Upgrade(w, r, wsconn.Options{QueuedBytes: cap, Shared: shared})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(in.ctx)
	c := &liveConn{in: in, ws: ws, ctx: ctx, cancel: cancel, public: public, epoch: r.Header.Get("X-Flats-Access-Epoch"), wake: make(chan struct{}, 1), commands: make(chan liveOutbound), snapshots: make(chan liveSnapshot)}
	in.live.mu.Lock()
	c.exposure = exposure
	if public && exposure != in.live.publicEpoch {
		in.live.mu.Unlock()
		cancel()
		ws.Abort()
		return
	}
	in.live.conns[c] = true
	in.live.mu.Unlock()
	lifetimeStop := context.AfterFunc(ctx, func() { ws.Close(1001, "unavailable") })
	defer lifetimeStop()
	routeStop := context.AfterFunc(r.Context(), func() {
		closing, _ := ws.CloseStatus()
		if ctx.Err() != nil || closing != 0 {
			return
		}
		if public {
			ws.Abort()
		} else {
			ws.Close(1008, "access_changed")
		}
		cancel()
		// Core withdraws epoch-bound routes explicitly; request cancellation can be a disconnect.
		if !public || c.epoch != "" {
			return
		}
		in.live.mu.Lock()
		for token, res := range in.live.resumes {
			if res.public == public && res.epoch == c.epoch {
				_ = res.spill.Close()
				delete(in.live.resumes, token)
			}
		}
		in.live.mu.Unlock()
	})
	defer func() {
		routeStop()
		cancel()
		ws.Close(1000, "")
		if c.hydration != nil {
			c.hydration()
		}
		c.hydrationWG.Wait()
		in.live.mu.Lock()
		if public {
			// Graceful Close may still be draining content after the handler exits.
			in.live.drainingPublic[ws] = true
			go func() {
				<-ws.Done()
				in.live.mu.Lock()
				delete(in.live.drainingPublic, ws)
				in.live.mu.Unlock()
			}()
		}
		delete(in.live.conns, c)
		in.live.mu.Unlock()
		in.broadcastPresence()
	}()
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(10 * time.Second))
	raw, err := ws.ReadMessage(liveClientFrame)
	if err != nil {
		return
	}
	_ = ws.NetConn().SetReadDeadline(time.Time{})
	var hello liveHello
	if err := strictDecode([]byte(raw), &hello); err != nil || hello.Type != "hello" || hello.Version != 1 || hello.Pending.Count < 0 || hello.NextWindow < 0 || len(hello.PendingScopes) > 128 {
		c.fatal("protocol")
		return
	}
	if public && (hello.Session != "" || hello.Pending.Count != 0 || hello.Name != "") {
		c.fatal("forbidden")
		return
	}
	if hello.Page != "" && hello.Page.ValidateKind(PageKind) != nil {
		c.fatal("protocol")
		return
	}
	for _, s := range hello.PendingScopes {
		if _, err := s.selection(public); err != nil {
			c.fatal("protocol")
			return
		}
	}
	if !public {
		if hello.Session != "" {
			c.session, err = in.ResumeSession(ctx, hello.Session)
		} else if hello.Pending.Count > 0 {
			err = &WriteError{Code: "conflict", Reason: "session_expired"}
		} else {
			editor, tokenErr := randomNotebookToken()
			if tokenErr != nil {
				err = tokenErr
			} else {
				label := hello.Name
				if label == "" {
					label = "Editor"
				}
				login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
				verified := login != ""
				if verified {
					editor = "tailscale:" + login
					label = r.Header.Get("Tailscale-User-Name")
					if label == "" {
						label = login
					}
				}
				c.session, err = in.NewSession(ctx, editor, label, verified)
			}
		}
		if err != nil {
			c.fatal(liveErrorCode(err))
			return
		}
	}
	if err := in.ReadTx(ctx, func(_ *sql.Tx, seq Sequence) error { c.seq = seq; c.baseSeq = seq; return nil }); err != nil {
		c.fatal("unavailable")
		return
	}
	welcome := map[string]any{"access": "public"}
	if !public {
		welcome = map[string]any{"access": "private", "session": c.session.Token, "session_id": c.session.ID, "expires": c.session.Expires, "me": map[string]any{"label": c.session.Label, "verified": c.session.Verified}}
	}
	if err := c.send(mustLiveEnvelope("welcome", welcome)); err != nil {
		return
	}
	c.generation = 1
	c.scopes = []Scope{{Tree: true, Public: public}}
	if hello.Page != "" {
		c.scopes = append(c.scopes, Scope{Page: hello.Page, Public: public})
	}
	for _, s := range hello.PendingScopes {
		scope, _ := s.selection(public)
		c.scopes = append(c.scopes, scope)
	}
	resumed := false
	if hello.Spill != "" && hello.Watermark != nil {
		in.live.mu.Lock()
		resume, ok := in.live.resumes[hello.Spill]
		in.live.mu.Unlock()
		if ok && resume.public == public && resume.epoch == c.epoch && resume.session == c.session.ID && *hello.Watermark <= c.seq && resume.spill.Sequence <= *hello.Watermark && (!public || resume.spill.Sequence == *hello.Watermark) {
			if _, ok := in.spills.Lookup(hello.Spill); ok && c.resumeCoverage(*hello.Watermark) {
				c.seq = *hello.Watermark
				if !slices.Contains(c.scopes, resume.spill.scope) {
					c.scopes = append(c.scopes, resume.spill.scope)
				}
				c.startHydration(&resume, hello.NextWindow)
				resumed = true
			}
		}
	}
	if !resumed {
		c.startHydration(nil, 0)
	}
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); c.readLoop(); c.cancel() }()
	in.live.mu.Lock()
	c.welcomed = true
	in.live.mu.Unlock()
	in.broadcastPresence()
	c.pump()
	cancel()
	ws.Close(1000, "")
	<-readerDone
}

func (c *liveConn) resumeCoverage(watermark Sequence) bool {
	if watermark == c.seq {
		return true
	}
	if c.public {
		return false
	}
	var count uint64
	if err := c.in.readers.QueryRowContext(c.ctx, `SELECT count(*) FROM deltas WHERE seq>? AND seq<=?`, sequenceString(watermark), sequenceString(c.seq)).Scan(&count); err != nil {
		return false
	}
	return count == uint64(c.seq-watermark)
}

func liveErrorCode(err error) string {
	var e *WriteError
	if errors.As(err, &e) {
		if e.Reason != "" {
			return e.Reason
		}
		return e.Code
	}
	return "unavailable"
}
func (c *liveConn) fatal(code string) {
	_ = c.ws.Send(string(mustLiveEnvelope("fatal", map[string]any{"code": code})))
	c.ws.Close(1008, code)
}
func (c *liveConn) command(cmd liveOutbound) bool {
	select {
	case c.commands <- cmd:
		return true
	case <-c.ctx.Done():
		return false
	}
}

func (c *liveConn) readLoop() {
	var assembly liveAssembly
	var refusedAssembly Sequence
	defer assembly.reset()
	var assemblyTimer *time.Timer
	defer func() {
		if assemblyTimer != nil {
			assemblyTimer.Stop()
		}
	}()
	for {
		raw, err := c.ws.ReadMessage(liveClientFrame)
		if err != nil {
			return
		}
		kind, fields, err := liveObject([]byte(raw))
		if err != nil {
			c.fatal("protocol")
			return
		}
		if c.public && kind != "watch" {
			var n Sequence
			_ = json.Unmarshal(fields["n"], &n)
			c.reject(&WriteError{Code: "forbidden"}, n)
			if n == 0 {
				return
			}
			continue
		}
		switch kind {
		case "watch":
			var w liveWatch
			if err := strictDecode([]byte(raw), &w); err != nil {
				c.reject(err, 0)
				continue
			}
			if _, err := w.scopes(c.public); err != nil {
				c.reject(err, 0)
				continue
			}
			if !c.command(liveOutbound{watch: &w}) {
				return
			}
		case "action", "action_part":
			// Charge an action before assembly/decode, including malformed input.
			// Multipart continuations share the token taken by their first part.
			var n Sequence
			if json.Unmarshal(fields["n"], &n) != nil || n == 0 {
				c.ws.Close(1008, "resend")
				return
			}
			if kind == "action_part" && refusedAssembly == n {
				var part int
				if json.Unmarshal(fields["part"], &part) == nil && part != 0 {
					continue
				}
				refusedAssembly = 0
			}
			if kind == "action" || assembly.parts == 0 {
				if !c.actions.take(c.in.now(), 30, 60) {
					if kind == "action_part" {
						refusedAssembly = n
					}
					c.reject(unavailable("action_rate"), n)
					continue
				}
			}
			data := []byte(raw)
			if kind == "action_part" {
				wasEmpty := assembly.parts == 0
				if assembly.parts != 0 && assembly.n != n {
					c.fatal("protocol")
					return
				}
				data, err = assembly.add(data, time.Now())
				if err != nil {
					assembly.reset()
				}
				if wasEmpty && assembly.parts != 0 {
					assemblyTimer = time.AfterFunc(30*time.Second, func() { c.cancel(); c.ws.Close(1008, "assembly_timeout") })
				}
				if assembly.parts == 0 && assemblyTimer != nil {
					assemblyTimer.Stop()
					assemblyTimer = nil
				}
				if err != nil {
					var failure *WriteError
					if errors.As(err, &failure) && failure.Retry {
						refusedAssembly = n
					}
					c.reject(err, n)
					continue
				}
				if data == nil {
					continue
				}
				kind, fields, err = liveObject(data)
				if err != nil || kind != "action" {
					c.reject(invalidRead("action"), n)
					continue
				}
			}
			delete(fields, "type")
			payload, _ := json.Marshal(fields)
			action, err := DecodeAction(payload)
			if err != nil {
				c.reject(invalidRead("action"), n)
				continue
			}
			ack, err := c.in.SubmitAction(c.ctx, c.session.Token, uint64(action.N), action)
			if err != nil {
				var failure *WriteError
				if errors.As(err, &failure) && failure.Reason == "action_gap" {
					c.fatal("protocol")
					return
				}
				c.reject(err, action.N)
				continue
			}
			if !c.command(liveOutbound{ack: &ack}) {
				return
			}
		case "presence":
			if !c.presences.take(time.Now(), 20, 20) {
				continue
			}
			if err := c.setPresence([]byte(raw)); err != nil {
				c.reject(err, 0)
			}
		default:
			c.reject(invalidRead("frame_type"), 0)
		}
	}
}

func (c *liveConn) reject(err error, n Sequence) {
	if n == 0 {
		c.fatal("protocol")
		return
	}
	var e *WriteError
	if !errors.As(err, &e) {
		e = &WriteError{Code: "invalid", Reason: "frame"}
	}
	value := map[string]any{"n": n, "code": e.Code, "retry": e.Retry, "detail": map[string]any{"reason": e.Reason, "message": e.Detail}}
	c.command(liveOutbound{kind: "reject", value: value})
}

func (c *liveConn) send(raw []byte) error {
	if len(raw) > liveServerFrame {
		return fmt.Errorf("notebook frame exceeds limit")
	}
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		if c.ws.Budget().Used()+int64(len(raw)) <= c.ws.Budget().Limit() {
			if c.public {
				c.in.live.mu.Lock()
				defer c.in.live.mu.Unlock()
				if c.exposure != c.in.live.publicEpoch || c.ctx.Err() != nil {
					return unavailable("access_changed")
				}
			}
			return c.ws.Send(string(raw))
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-c.ws.Done():
			return errors.New("socket closed")
		case <-ticker.C:
		}
	}
}

func (c *liveConn) pump() {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	var pending *liveSnapshot
	for {
		if c.ctx.Err() != nil {
			return
		}
		if err := c.deliverDeltas(); err != nil {
			c.fatal(liveErrorCode(err))
			return
		}
		if pending != nil && (!c.currentSnapshot(*pending) || c.ws.Budget().Used() == 0) {
			if !c.installSnapshot(*pending) {
				return
			}
			pending = nil
		}
		if raw := c.takePresence(); raw != nil {
			if err := c.send(raw); err != nil {
				return
			}
		}
		snapshots := c.snapshots
		if pending != nil {
			snapshots = nil
		}
		select {
		case <-c.ctx.Done():
			return
		case <-c.ws.Done():
			return
		case <-c.wake:
		case <-tick.C:
		case cmd := <-c.commands:
			if cmd.watch != nil {
				if cmd.watch.Generation <= c.generation {
					continue
				}
				c.generation = cmd.watch.Generation
				c.scopes, _ = cmd.watch.scopes(c.public)
				c.startHydration(nil, 0)
			} else if cmd.ack != nil {
				if err := c.deliverDeltas(); err != nil {
					c.fatal(liveErrorCode(err))
					return
				}
				if cmd.ack.Seq <= c.baseSeq && cmd.ack.Summary.Applied > 0 {
					c.startReceipt(cmd.ack)
				} else if err := c.send(mustLiveEnvelope("ack", cmd.ack)); err != nil {
					return
				}
			} else if err := c.send(mustLiveEnvelope(cmd.kind, cmd.value)); err != nil {
				return
			}
		case frame := <-snapshots:
			pending = &frame
		}
	}
}

func (c *liveConn) currentSnapshot(frame liveSnapshot) bool {
	if frame.generation != c.generation {
		return false
	}
	if frame.receipt != nil {
		_, ok := c.receipts[frame.receipt]
		return ok
	}
	return frame.run == c.hydrationRun
}

func (c *liveConn) installSnapshot(frame liveSnapshot) bool {
	if frame.delivered != nil {
		defer close(frame.delivered)
	}
	if !c.currentSnapshot(frame) {
		return true
	}
	if frame.ack != nil {
		c.receipts[frame.ack]()
		delete(c.receipts, frame.ack)
		if err := c.send(mustLiveEnvelope("ack", frame.ack)); err != nil {
			return false
		}
		if len(c.receipts) == 0 {
			c.startHydration(nil, 0)
		}
		return true
	}
	if frame.database != "" {
		return c.hydratedNotices(frame.database, frame.seq) == nil
	}
	if frame.err != nil {
		c.fatal(liveErrorCode(frame.err))
		c.cancel()
		return false
	}
	return c.send(frame.data) == nil
}

func (c *liveConn) startHydration(resume *liveResume, next int) {
	if len(c.receipts) != 0 {
		acks := make([]*Ack, 0, len(c.receipts))
		for ack, cancel := range c.receipts {
			cancel()
			acks = append(acks, ack)
		}
		clear(c.receipts)
		for _, ack := range acks {
			c.startReceipt(ack)
		}
		return
	}
	c.hydrate(resume, next)
}

func (c *liveConn) startReceipt(ack *Ack) {
	for pending := range c.receipts {
		if pending.N == ack.N {
			return
		}
	}
	if c.hydration != nil {
		c.hydration()
		c.hydration = nil
		c.hydrationRun++
	}
	if c.receipts == nil {
		c.receipts = map[*Ack]context.CancelFunc{}
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.receipts[ack] = cancel
	generation := c.generation
	scopes := slices.Clone(c.scopes)
	c.hydrationWG.Add(1)
	go func() {
		defer c.hydrationWG.Done()
		emit := func(f liveSnapshot) bool {
			f.receipt = ack
			return c.emitSnapshot(ctx, f)
		}
		if err := c.hydrateReceipt(ctx, generation, ack, scopes, emit); err != nil && ctx.Err() == nil {
			emit(liveSnapshot{generation: generation, err: err})
		}
	}()
}

func (c *liveConn) emitSnapshot(ctx context.Context, f liveSnapshot) bool {
	if f.data != nil || f.ack != nil || f.database != "" {
		f.delivered = make(chan struct{})
	}
	select {
	case c.snapshots <- f:
	case <-ctx.Done():
		return false
	}
	if f.delivered != nil {
		select {
		case <-f.delivered:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (c *liveConn) hydrate(resume *liveResume, next int) {
	if c.hydration != nil {
		c.hydration()
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.hydration = cancel
	generation := c.generation
	c.hydrationRun++
	run := c.hydrationRun
	scopes := slices.Clone(c.scopes)
	c.hydrationWG.Add(1)
	go func() {
		defer c.hydrationWG.Done()
		emit := func(f liveSnapshot) bool {
			f.run = run
			return c.emitSnapshot(ctx, f)
		}
		if resume != nil {
			if err := c.streamSpill(ctx, generation, resume.spill, next, emit); err != nil && ctx.Err() == nil {
				emit(liveSnapshot{generation: generation, err: err})
			}
			return
		}
		for _, scope := range scopes {
			if c.public && !c.waitPublicStart(ctx) {
				return
			}
			spill, err := c.in.spills.Capture(ctx, c.in.ReadTx, scope, nil)
			if err != nil && scope.Database != "" {
				if c.emptyDatabase(ctx, generation, scope, emit) == nil {
					continue
				}
			}
			if err != nil {
				if ctx.Err() == nil {
					emit(liveSnapshot{generation: generation, err: err})
				}
				return
			}
			c.in.live.mu.Lock()
			if ctx.Err() != nil || c.public && c.exposure != c.in.live.publicEpoch {
				c.in.live.mu.Unlock()
				_ = spill.Close()
				return
			}
			// Retain no more handles than the manager has live spills.
			for token := range c.in.live.resumes {
				if _, ok := c.in.spills.Lookup(token); !ok {
					delete(c.in.live.resumes, token)
				}
			}
			c.in.live.resumes[spill.Cursor()] = liveResume{spill: spill, session: c.session.ID, epoch: c.epoch, public: c.public}
			c.in.live.mu.Unlock()
			if err := c.streamSpill(ctx, generation, spill, 0, emit); err != nil {
				if ctx.Err() == nil {
					emit(liveSnapshot{generation: generation, err: err})
				}
				return
			}
		}
	}()
}

func (c *liveConn) waitPublicStart(ctx context.Context) bool {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		c.in.live.mu.Lock()
		ok := c.in.live.starts.take(time.Now(), 8, 8)
		c.in.live.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

func (c *liveConn) streamSpill(ctx context.Context, generation int, s *Spill, next int, emit func(liveSnapshot) bool) error {
	return c.streamSpillSelected(ctx, generation, s, next, nil, emit)
}

func (c *liveConn) streamSpillSelected(ctx context.Context, generation int, s *Spill, next int, windows map[int]bool, emit func(liveSnapshot) bool) error {
	wireScope := liveScope{Kind: "tree"}
	if s.scope.Page != "" {
		wireScope = liveScope{Kind: "page", ID: s.scope.Page}
	}
	if s.scope.Database != "" {
		wireScope = liveScope{Kind: "database", ID: s.scope.Database}
	}
	completed := false
	defer func() {
		if completed {
			_ = s.Close()
			c.in.live.mu.Lock()
			delete(c.in.live.resumes, s.Cursor())
			c.in.live.mu.Unlock()
		}
	}()
	ranges, err := liveWindowRanges(s, windows)
	if err != nil {
		return err
	}
	for _, parts := range ranges {
		for i := parts[0]; i < parts[1]; i++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			raw, _, err := s.Part(i)
			if err != nil {
				return err
			}
			var frame map[string]json.RawMessage
			if err := json.Unmarshal(raw, &frame); err != nil {
				return err
			}
			var window int
			if err := json.Unmarshal(frame["window"], &window); err != nil {
				return err
			}
			if window >= next && (windows == nil || windows[window]) {
				if windows != nil {
					frame["final_window"] = json.RawMessage(`false`)
				}
				frame["type"], _ = json.Marshal("snapshot")
				frame["generation"], _ = json.Marshal(generation)
				frame["scope"], _ = json.Marshal(wireScope)
				frame["spill"], _ = json.Marshal(s.Cursor())
				if s.scope.Database == "" {
					delete(frame, "window")
					delete(frame, "rows")
					delete(frame, "final_window")
				}
				data, err := json.Marshal(frame)
				if err != nil {
					return err
				}
				if len(data) > liveServerFrame {
					return errors.New("snapshot frame exceeds limit")
				}
				if !emit(liveSnapshot{generation: generation, data: data}) {
					return ctx.Err()
				}
			}
		}
	}
	if windows == nil && !c.public && s.scope.Database != "" && !emit(liveSnapshot{generation: generation, database: s.scope.Database, seq: s.Sequence}) {
		return ctx.Err()
	}
	completed = true
	return nil
}

// The durable log is the delta queue: wakeups coalesce, but committed sequences do not.
func (c *liveConn) deliverDeltas() error {
	var currentRaw, minRaw string
	var bytesBehind int64
	err := c.in.readers.QueryRowContext(c.ctx, `SELECT seq,(SELECT coalesce(min(seq),'0') FROM deltas),(SELECT coalesce(sum(bytes),0) FROM deltas WHERE seq>?) FROM meta WHERE singleton=1`, sequenceString(c.seq)).Scan(&currentRaw, &minRaw, &bytesBehind)
	if err != nil {
		return err
	}
	n, err := parseDBDecimal(currentRaw)
	if err != nil {
		return err
	}
	current := Sequence(n)
	if current <= c.seq {
		return nil
	}
	oldest, err := parseDBDecimal(minRaw)
	if err != nil {
		return err
	}
	cap := int64(livePrivateQueue)
	if c.public {
		cap = livePublicQueue
	}
	if oldest == 0 || oldest > uint64(c.seq)+1 || bytesBehind > cap {
		if c.resynced {
			return unavailable("live_backpressure")
		}
		c.resynced = true
		c.generation++
		c.seq = current
		c.baseSeq = current
		if err := c.send(mustLiveEnvelope("resync", map[string]any{"generation": c.generation, "seq": current})); err != nil {
			return err
		}
		c.startHydration(nil, 0)
		return nil
	}
	for c.seq < current {
		target := c.seq + 1
		var effects, source string
		err := c.in.readers.QueryRowContext(c.ctx, `SELECT effects,source FROM deltas WHERE seq=?`, sequenceString(target)).Scan(&effects, &source)
		if errors.Is(err, sql.ErrNoRows) {
			if c.resynced {
				return unavailable("live_backpressure")
			}
			c.resynced = true
			c.generation++
			c.seq = current
			c.baseSeq = current
			if err := c.send(mustLiveEnvelope("resync", map[string]any{"generation": c.generation, "seq": current})); err != nil {
				return err
			}
			c.startHydration(nil, 0)
			return nil
		} else if err != nil {
			return err
		}
		raw, err := c.projectDelta(target, []byte(effects), source)
		if err != nil {
			return err
		}
		if err := liveParts(raw, "delta_part", c.generation, target, c.send); err != nil {
			return err
		}
		c.seq = target
		if !c.public {
			if err := c.privateNotices(target, effects, source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *liveConn) privateNotices(seq Sequence, effects, source string) error {
	if source == "" {
		var delta struct {
			Changes []struct {
				Ref Ref `json:"ref"`
			} `json:"changes"`
		}
		if err := json.Unmarshal([]byte(effects), &delta); err != nil {
			return err
		}
		if len(delta.Changes) > 0 {
			changed := make([]ID, 0, min(len(delta.Changes), 64))
			for _, item := range delta.Changes {
				if len(changed) == 64 {
					break
				}
				changed = append(changed, item.Ref.ID)
			}
			if err := c.send(mustLiveEnvelope("activity", map[string]any{"message": "The agent updated the notebook", "changed": changed})); err != nil {
				return err
			}
		}
	}
	var after ID
	for {
		var id ID
		var removed string
		err := c.in.readers.QueryRowContext(c.ctx, `SELECT view_id,removed FROM view_notices WHERE seq=? AND view_id>? ORDER BY view_id LIMIT 1`, sequenceString(seq), after).Scan(&id, &removed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.sendViewNotice(id, seq, removed); err != nil {
			return err
		}
		after = id
	}
}

func (in *Instance) initLiveTracking(ctx context.Context) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS notebook_receipt_units(session TEXT NOT NULL,n TEXT COLLATE notebook_u64 NOT NULL,kind TEXT NOT NULL,scope TEXT NOT NULL,item TEXT NOT NULL,PRIMARY KEY(session,n,kind,scope,item),FOREIGN KEY(session,n) REFERENCES receipts(session,n) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS notebook_block_sources(block TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,version TEXT COLLATE notebook_u64 NOT NULL,source TEXT NOT NULL)`,
		`CREATE TEMP TABLE notebook_live_places(id TEXT PRIMARY KEY,page TEXT NOT NULL)`,
		`CREATE TEMP TRIGGER notebook_live_move BEFORE UPDATE OF page ON main.blocks BEGIN INSERT OR IGNORE INTO notebook_live_places(id,page) VALUES(OLD.id,OLD.page); END`,
	} {
		if _, err := in.db.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

type deltaLocation struct {
	PreviousPage ID `json:"previous_page,omitempty"`
	Page         ID `json:"page,omitempty"`
	Database     ID `json:"database,omitempty"`
}

func locateDelta(ctx context.Context, q dbReader, item Item) (deltaLocation, error) {
	var loc deltaLocation
	switch d := item.Data.(type) {
	case Page:
		loc.Page = item.ID()
	case Block:
		loc.Page = d.Page
	case Database:
		loc.Database = item.ID()
	case Property:
		loc.Database = d.Database
	case Row:
		loc.Database = d.Database
	case View:
		loc.Database = d.Database
	case Option:
		if err := q.QueryRowContext(ctx, `SELECT database_id FROM properties WHERE id=?`, d.Property).Scan(&loc.Database); err != nil {
			return loc, err
		}
	}
	if loc.Database != "" {
		if err := q.QueryRowContext(ctx, `SELECT b.page FROM databases d JOIN blocks b ON b.id=d.block WHERE d.id=?`, loc.Database).Scan(&loc.Page); err != nil {
			return loc, err
		}
	}
	return loc, nil
}

func (c *liveConn) projectDelta(seq Sequence, effects []byte, source string) ([]byte, error) {
	var payload struct {
		Changes   []json.RawMessage    `json:"changes"`
		Overwrote []json.RawMessage    `json:"overwrote"`
		Locations map[ID]deltaLocation `json:"locations"`
	}
	if err := json.Unmarshal(effects, &payload); err != nil {
		return nil, err
	}
	tree, changes, scopes := []json.RawMessage{}, []json.RawMessage{}, []any{}
	for _, raw := range payload.Changes {
		var header struct {
			Ref     Ref  `json:"ref"`
			Deleted bool `json:"deleted"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, err
		}
		var item Item
		var err error
		if !header.Deleted {
			item, err = DecodeItem(raw)
		} else {
			if _, ok := payload.Locations[header.Ref.ID]; ok {
				item = Item{Ref: header.Ref, Ver: Version(seq)}
			} else {
				var record itemRecord
				record, err = loadRecord(c.ctx, c.in.readers, header.Ref.ID)
				item = record.Item
			}
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		visible := item.Kind() == PageKind
		location, ok := payload.Locations[item.ID()]
		if !ok {
			var err error
			location, err = locateDelta(c.ctx, c.in.readers, item)
			if err != nil {
				return nil, err
			}
		}
		page, db := location.Page, location.Database
		for _, s := range c.scopes {
			if s.Page != "" && s.Page == page && item.Kind() != RowKind {
				visible = true
			}
			if s.Database != "" && s.Database == db {
				visible = true
			}
		}
		if !visible && location.PreviousPage != "" {
			for _, s := range c.scopes {
				if s.Page == location.PreviousPage {
					var deletion []byte
					if c.public {
						deletion, _ = json.Marshal(map[string]any{"id": item.ID(), "kind": item.Kind(), "deleted": true})
					} else {
						deletion, _ = json.Marshal(map[string]any{"ref": item.Ref, "deleted": true})
					}
					changes = append(changes, deletion)
					break
				}
			}
		}
		if !visible {
			scope := map[string]any{"kind": "page", "id": page, "invalidated": true}
			if db != "" {
				scope["kind"] = "database"
				scope["id"] = db
			}
			if !c.public {
				scope["ver"] = item.Ver
			}
			scopes = append(scopes, scope)
			continue
		}
		encoded := raw
		if c.public {
			if header.Deleted {
				encoded, _ = json.Marshal(map[string]any{"id": header.Ref.ID, "kind": header.Ref.ID.Kind(), "deleted": true})
			} else {
				var out bytes.Buffer
				if err := writePublicItem(&out, item); err != nil {
					return nil, err
				}
				encoded = bytes.TrimSpace(out.Bytes())
			}
		}
		if item.Kind() == PageKind {
			tree = append(tree, encoded)
		} else {
			changes = append(changes, encoded)
		}
	}
	frame := map[string]any{"generation": c.generation, "seq": seq, "tree": tree, "scopes": scopes, "changes": changes}
	if !c.public {
		if json.Valid([]byte(source)) && strings.HasPrefix(source, "{") {
			frame["from"] = json.RawMessage(source)
		}
		if len(payload.Overwrote) > 0 {
			frame["overwrote"] = payload.Overwrote
		}
	}
	return liveEnvelope("delta", frame)
}

// Receipt dependencies outlive the delta log and contain no content or resume secrets.
func (c *liveConn) hydrateReceipt(ctx context.Context, generation int, ack *Ack, pendingScopes []Scope, emit func(liveSnapshot) bool) error {
	hash := contentHash([]byte(c.session.Token))
	rows, err := c.in.readers.QueryContext(ctx, `SELECT DISTINCT kind,scope FROM notebook_receipt_units WHERE session=? AND n=? AND kind!='receipt' ORDER BY kind,scope`, hash, sequenceString(ack.N))
	if err != nil {
		return err
	}
	var scopes []Scope
	for rows.Next() {
		var kind string
		var id ID
		if err := rows.Scan(&kind, &id); err != nil {
			rows.Close()
			return err
		}
		scope, err := (liveScope{Kind: kind, ID: id}).selection(false)
		if err != nil {
			rows.Close()
			return err
		}
		scopes = append(scopes, scope)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// A complete page unit reconciles schema absence without reconciling database rows.
	removed, err := c.in.readers.QueryContext(ctx, `SELECT DISTINCT b.page FROM notebook_receipt_units u JOIN databases d ON d.id=u.scope JOIN blocks b ON b.id=d.block WHERE u.session=? AND u.n=? AND u.kind='database' AND substr(u.item,1,instr(u.item,'_')) IN ('pr_','o_','v_') ORDER BY b.page`, hash, sequenceString(ack.N))
	if err != nil {
		return err
	}
	for removed.Next() {
		var page ID
		if err := removed.Scan(&page); err != nil {
			removed.Close()
			return err
		}
		if scope := (Scope{Page: page}); !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	err = removed.Err()
	removed.Close()
	if err != nil {
		return err
	}
	legacy := false
	if len(scopes) == 0 {
		var tracked bool
		if err := c.in.readers.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notebook_receipt_units WHERE session=? AND n=? AND kind='receipt')`, hash, sequenceString(ack.N)).Scan(&tracked); err != nil {
			return err
		}
		if !tracked {
			// Older receipts have no per-unit dependencies, so retain their conservative base requirement.
			legacy = true
			scopes = slices.Clone(pendingScopes)
		}
	}
	for _, scope := range scopes {
		if scope.Database != "" && !legacy {
			var page ID
			var rowsRequired bool
			err := c.in.readers.QueryRowContext(ctx, `SELECT b.page,EXISTS(SELECT 1 FROM notebook_receipt_units WHERE session=? AND n=? AND kind='database' AND scope=d.id AND substr(item,1,2)='r_') FROM databases d JOIN items i ON i.id=d.id JOIN blocks b ON b.id=d.block WHERE d.id=? AND i.alive=1`, hash, sequenceString(ack.N), scope.Database).Scan(&page, &rowsRequired)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && !rowsRequired && slices.Contains(scopes, Scope{Page: page}) {
				continue
			}
		}
		var windows map[int]bool
		read := func(ctx context.Context, fn func(*sql.Tx, Sequence) error) error {
			return c.in.ReadTx(ctx, func(tx *sql.Tx, seq Sequence) error {
				if err := fn(tx, seq); err != nil {
					return err
				}
				if scope.Database == "" || legacy {
					return nil
				}
				windows = map[int]bool{0: true}
				all := false
				ids, err := tx.QueryContext(ctx, `SELECT item FROM notebook_receipt_units WHERE session=? AND n=? AND kind='database' AND scope=?`, hash, sequenceString(ack.N), scope.Database)
				if err != nil {
					return err
				}
				defer ids.Close()
				for ids.Next() {
					var id ID
					if err := ids.Scan(&id); err != nil {
						return err
					}
					if id.Kind() != RowKind {
						continue
					}
					var rank int64
					err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM rows r JOIN items i ON i.id=r.id WHERE r.database_id=t.database_id AND i.alive=1 AND (r.position<t.position OR (r.position=t.position AND r.id<t.id))) FROM rows t JOIN items i ON i.id=t.id WHERE t.id=? AND t.database_id=? AND i.alive=1`, id, scope.Database).Scan(&rank)
					if errors.Is(err, sql.ErrNoRows) {
						all = true
						continue
					}
					if err != nil {
						return err
					}
					windows[int(rank/1000)+1] = true
				}
				if err := ids.Err(); err != nil {
					return err
				}
				if all {
					windows = nil
				}
				return nil
			})
		}
		spill, err := c.in.spills.Capture(ctx, read, scope, nil)
		if err != nil && scope.Database != "" {
			if c.emptyDatabase(ctx, generation, scope, emit) == nil {
				continue
			}
		}
		if err != nil {
			return err
		}
		if err := c.streamSpillSelected(ctx, generation, spill, 0, windows, emit); err != nil {
			_ = spill.Close()
			return err
		}
	}
	if !emit(liveSnapshot{generation: generation, ack: ack}) {
		return ctx.Err()
	}
	return nil
}

func (c *liveConn) sendViewNotice(id ID, seq Sequence, removed string) error {
	if c.noticeSeen == nil {
		c.noticeSeen = map[liveNotice]bool{}
	}
	key := liveNotice{id, seq}
	if c.noticeSeen[key] {
		return nil
	}
	if err := c.send(mustLiveEnvelope("view_adjustment", map[string]any{"view": id, "notice_seq": seq, "removed": json.RawMessage(removed)})); err != nil {
		return err
	}
	c.noticeSeen[key] = true
	return nil
}

func (c *liveConn) hydratedNotices(database ID, through Sequence) error {
	if c.public {
		return nil
	}
	var after ID
	var seq Sequence
	for {
		var id ID
		var raw, removed string
		err := c.in.readers.QueryRowContext(c.ctx, `SELECT n.view_id,n.seq,n.removed FROM view_notices n JOIN views v ON v.id=n.view_id JOIN items i ON i.id=v.id WHERE v.database_id=? AND i.alive=1 AND n.seq<=? AND (n.view_id>? OR (n.view_id=? AND n.seq>?)) ORDER BY n.view_id,n.seq LIMIT 1`, database, sequenceString(through), after, after, sequenceString(seq)).Scan(&id, &raw, &removed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err := parseDBDecimal(raw)
		if err != nil {
			return err
		}
		seq = Sequence(n)
		after = id
		if err := c.sendViewNotice(id, seq, removed); err != nil {
			return err
		}
	}
}
func liveWindowRanges(s *Spill, windows map[int]bool) ([][2]int, error) {
	if windows == nil {
		return [][2]int{{0, s.parts}}, nil
	}
	var selected []int
	for w := range windows {
		selected = append(selected, w)
	}
	slices.Sort(selected)
	m := s.manager
	m.mu.Lock()
	if s.removed {
		m.mu.Unlock()
		return nil, errSpillExpired
	}
	f, err := os.Open(filepath.Join(s.dir, "parts"))
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	s.handles[f] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); _ = f.Close(); delete(s.handles, f); m.mu.Unlock() }()
	ranges := make([][2]int, 0, len(selected))
	for _, window := range selected {
		w, err := readSpillWindow(f, int64(window))
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, [2]int{int(w.first), int(w.first + w.parts)})
	}
	return ranges, nil
}
func (c *liveConn) emptyDatabase(ctx context.Context, generation int, scope Scope, emit func(liveSnapshot) bool) error {
	var seq Sequence
	err := c.in.ReadTx(ctx, func(tx *sql.Tx, current Sequence) error {
		seq = current
		var alive bool
		err := tx.QueryRowContext(ctx, `SELECT alive FROM items WHERE id=?`, scope.Database).Scan(&alive)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if alive {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return err
	}
	token, err := randomNotebookToken()
	if err != nil {
		return err
	}
	data := mustLiveEnvelope("snapshot", map[string]any{"generation": generation, "seq": seq, "scope": liveScope{Kind: "database", ID: scope.Database}, "spill": token, "window": 0, "rows": 0, "final_window": true, "spill_part": 0, "part": 0, "parts": 1, "data": `{"items":[]}`})
	if !emit(liveSnapshot{generation: generation, data: data}) {
		return ctx.Err()
	}
	return nil
}
