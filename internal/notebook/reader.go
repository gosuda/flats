package notebook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const spillChunk = 64 << 10
const spillNodeSize = 72

func (c *spillCapture) captureDatabase(ctx context.Context, tx *sql.Tx, scope Scope) error {
	if err := c.addRecord(ctx, tx, scope.Database, false); err != nil {
		return err
	}
	if !c.callback {
		c.schema = Schema{Database: scope.Database, Options: map[ID][]Option{}}
	}
	if !c.callback && scope.View != nil {
		defer func() {
			if c.ranks != nil {
				c.closeQueryFile(c.ranks)
				_ = c.s.charge(-c.rankSlots * spillRankSlot)
			}
		}()
		c.schema.optionRank = func(property, option ID) int {
			if c.ranks == nil {
				return 0
			}
			rank, err := c.queryRank(option, nil)
			if err != nil && c.queryError == nil {
				c.queryError = err
			}
			return rank
		}
	}
	wanted := map[ID]bool{}
	for _, key := range c.view.Sort {
		if key.Property.Ref != nil {
			wanted[key.Property.Ref.ID] = true
		}
	}
	for _, key := range c.view.Filter {
		if key.Property.Ref != nil {
			wanted[key.Property.Ref.ID] = true
		}
	}
	if err := spillOrder(ctx, tx, scope.Database, PropertyOrderGroup, func(id ID) error {
		if err := c.addRecord(ctx, tx, id, false); err != nil {
			return err
		}
		if wanted[id] && !c.callback {
			var typ PropertyType
			source := &spillColumnReader{ctx: ctx, q: tx, query: `SELECT substr(CAST(data AS BLOB),?,?) FROM items WHERE id=?`, args: []any{id}}
			definition, err := spillFieldReader(source, "definition")
			if err != nil {
				return err
			}
			if err := readRecordField(definition, "type", &typ); err != nil {
				return err
			}
			if (typ == SelectType || typ == MultiSelectType) && c.ranks == nil {
				if err := c.prepareQueryRanks(ctx, tx, scope.Database); err != nil {
					return err
				}
			}
			property := Property{Database: scope.Database, Definition: PropertyDef{Type: typ}}
			c.schema.PropertyIDs = append(c.schema.PropertyIDs, id)
			c.schema.Properties = append(c.schema.Properties, property)
		}
		rank := 0
		return spillOrder(ctx, tx, id, OptionOrderGroup, func(option ID) error {
			if c.ranks != nil {
				if _, err := c.queryRank(option, &rank); err != nil {
					return err
				}
			}
			rank++
			return c.addRecord(ctx, tx, option, false)
		})
	}); err != nil {
		return err
	}
	if err := spillOrder(ctx, tx, scope.Database, ViewOrderGroup, func(id ID) error { return c.addRecord(ctx, tx, id, false) }); err != nil {
		return err
	}
	c.schemaNodes = c.nodes
	rows, err := tx.QueryContext(ctx, `SELECT r.id FROM rows r JOIN items i ON i.id=r.id WHERE r.database_id=? AND i.alive=1 ORDER BY r.position,r.id`, scope.Database)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id ID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if err := c.addRecord(ctx, tx, id, true); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Custom ordering callbacks receive the original materialized schema contract;
// the built-in query engine needs only queried property types and option ranks.
func (c *spillCapture) callbackSchema(ctx context.Context, tx *sql.Tx, database ID) error {
	c.callback = true
	c.schema = Schema{Database: database, Options: map[ID][]Option{}}
	return spillOrder(ctx, tx, database, PropertyOrderGroup, func(id ID) error {
		r, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		property := r.Item.Data.(Property)
		c.schema.PropertyIDs = append(c.schema.PropertyIDs, id)
		c.schema.Properties = append(c.schema.Properties, property)
		for _, id := range property.OptionOrder {
			r, err := loadRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			c.schema.Options[r.Item.Data.(Option).Property] = append(c.schema.Options[r.Item.Data.(Option).Property], r.Item.Data.(Option))
		}
		return nil
	})
}

func spillOrder(ctx context.Context, tx *sql.Tx, owner ID, kind GroupKey, visit func(ID) error) error {
	r := &spillColumnReader{ctx: ctx, q: tx, query: `SELECT substr(CAST(ids AS BLOB),?,?) FROM orders WHERE owner_id=? AND kind=?`, args: []any{owner, kind}}
	dec := json.NewDecoder(r)
	if token, err := dec.Token(); err != nil {
		return err
	} else if token != json.Delim('[') {
		return fmt.Errorf("invalid stored order")
	}
	for dec.More() {
		var id ID
		if err := dec.Decode(&id); err != nil {
			return err
		}
		if err := visit(id); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

func (c *spillCapture) prepareReferences(ctx context.Context, tx *sql.Tx, scope Scope) error {
	query, args := scopeIDs(scope)
	var count int64
	if err := tx.QueryRowContext(ctx, `WITH selected AS (`+query+`) SELECT count(*) FROM items i JOIN selected s ON s.id=i.id WHERE i.kind<>'row'`, args...).Scan(&count); err != nil {
		return err
	}
	c.refSlots = 1
	for c.refSlots < count*2 {
		c.refSlots *= 2
	}
	if err := c.s.charge(c.refSlots * readRefSlotBytes); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.s.dir, "agent-refs"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	c.refs = f
	c.s.manager.mu.Lock()
	c.s.handles[f] = true
	c.s.manager.mu.Unlock()
	return f.Truncate(c.refSlots * readRefSlotBytes)
}

func (c *spillCapture) indexReference(n spillNode) error {
	if c.refs == nil {
		return nil
	}
	ref, err := readRecordReference(io.NewSectionReader(c.s.data, n.start, n.length))
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(ref.ID))
	at := int64(binary.LittleEndian.Uint64(hash[:8]) & uint64(c.refSlots-1))
	var slot [readRefSlotBytes]byte
	for probes := int64(0); probes < c.refSlots; probes++ {
		if _, err := c.refs.ReadAt(slot[:], at*readRefSlotBytes); err != nil {
			return err
		}
		if slot[0] == 0 {
			slot[0] = byte(len(ref.ID))
			copy(slot[1:32], ref.ID)
			binary.LittleEndian.PutUint64(slot[32:40], uint64(ref.Inc))
			_, err := c.refs.WriteAt(slot[:], at*readRefSlotBytes)
			return err
		}
		at = (at + 1) & (c.refSlots - 1)
	}
	return fmt.Errorf("snapshot reference index full")
}

// Query text stays in its source column or immutable scratch range. Only the
// string decoder's fixed-size input buffer is retained across comparisons.
type spillQueryText struct {
	raw func() io.Reader
	err *error
}

func spillFieldReader(source io.Reader, field string) (*bufio.Reader, error) {
	r := bufio.NewReaderSize(source, spillChunk)
	nonspace := func() (byte, error) {
		for {
			b, err := r.ReadByte()
			if err != nil || !strings.ContainsRune(" 	\r\n", rune(b)) {
				return b, err
			}
		}
	}
	if b, err := nonspace(); err != nil {
		return nil, err
	} else if b != '{' {
		return nil, fmt.Errorf("invalid query object")
	}
	for {
		b, err := nonspace()
		if err != nil {
			return nil, err
		}
		if b == '}' {
			return nil, fmt.Errorf("query field missing: %s", field)
		}
		if err := r.UnreadByte(); err != nil {
			return nil, err
		}
		var raw bytes.Buffer
		if err := scanReadValue(r, &raw); err != nil {
			return nil, err
		}
		var name string
		if err := json.Unmarshal(raw.Bytes(), &name); err != nil {
			return nil, err
		}
		if b, err := nonspace(); err != nil {
			return nil, err
		} else if b != ':' {
			return nil, fmt.Errorf("invalid query field")
		}
		if name == field {
			return r, nil
		}
		if err := scanReadValue(r, nil); err != nil {
			return nil, err
		}
		if b, err := nonspace(); err != nil {
			return nil, err
		} else if b != ',' {
			return nil, fmt.Errorf("query field missing: %s", field)
		}
	}
}

func (t *spillQueryText) open() *spillQueryRunes {
	out := &spillQueryRunes{err: t.err}
	r, err := spillFieldReader(t.raw(), "value")
	if err != nil {
		out.fail(err)
		return out
	}
	out.r = r
	b, err := r.ReadByte()
	for err == nil && strings.ContainsRune(" 	\r\n", rune(b)) {
		b, err = r.ReadByte()
	}
	if err != nil {
		out.fail(err)
		return out
	}
	if b == 'n' {
		out.done = true
		return out
	}
	if b != '"' {
		out.fail(fmt.Errorf("invalid query text"))
	}
	return out
}

type spillQueryRunes struct {
	r    *bufio.Reader
	err  *error
	done bool
}

func (r *spillQueryRunes) fail(err error) {
	r.done = true
	if r.err != nil && *r.err == nil {
		*r.err = err
	}
}
func (r *spillQueryRunes) next() (rune, bool) {
	if r.done {
		return 0, false
	}
	v, _, err := r.r.ReadRune()
	if err != nil {
		r.fail(err)
		return 0, false
	}
	if v == '"' {
		r.done = true
		return 0, false
	}
	if v == '\\' {
		b, err := r.r.ReadByte()
		if err != nil {
			r.fail(err)
			return 0, false
		}
		switch b {
		case '"', '\\', '/':
			v = rune(b)
		case 'b':
			v = '\b'
		case 'f':
			v = '\f'
		case 'n':
			v = '\n'
		case 'r':
			v = '\r'
		case 't':
			v = '\t'
		case 'u':
			var hex [4]byte
			if _, err := io.ReadFull(r.r, hex[:]); err != nil {
				r.fail(err)
				return 0, false
			}
			n, err := strconv.ParseUint(string(hex[:]), 16, 16)
			if err != nil {
				r.fail(err)
				return 0, false
			}
			v = rune(n)
			if utf16.IsSurrogate(v) {
				var pair [6]byte
				if _, err := io.ReadFull(r.r, pair[:]); err != nil {
					r.fail(err)
					return 0, false
				}
				if pair[0] != '\\' || pair[1] != 'u' {
					r.fail(fmt.Errorf("invalid query surrogate"))
					return 0, false
				}
				n, err := strconv.ParseUint(string(pair[2:]), 16, 16)
				if err != nil {
					r.fail(err)
					return 0, false
				}
				v = utf16.DecodeRune(v, rune(n))
			}
		default:
			r.fail(fmt.Errorf("invalid query escape"))
			return 0, false
		}
	}
	if folded, ok := dbQueryFolds[v]; ok {
		v = folded
	}
	return v, true
}

type spillNode struct{ start, length, left, right, height, rows, bytes, keyStart, keyLength int64 }

type captureWriter struct {
	s      *Spill
	out    io.Writer
	offset int64
}

func (w *captureWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), spillChunk)]
		if err := w.s.charge(int64(len(chunk))); err != nil {
			return total, err
		}
		n, err := w.out.Write(chunk)
		w.offset += int64(n)
		total += n
		p = p[n:]
		if e := w.s.progress(n); err == nil {
			err = e
		}
		if err != nil {
			return total, err
		}
		if n < len(chunk) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// Capture captures one consistent transaction, then releases it before delivery.
func (m *SpillManager) Capture(ctx context.Context, read func(context.Context, func(*sql.Tx, Sequence) error) error, scope Scope, order func(Schema, ViewDef, []RowValues) []ID) (*Spill, error) {
	if err := validateSpillScope(scope); err != nil {
		return nil, err
	}
	captureCtx, cancel := context.WithCancelCause(ctx)
	request := &spillRequest{scope: scope, cancel: cancel, estimating: true}
	m.mu.Lock()
	request.started = m.now()
	m.requests[request] = true
	m.scheduleLocked()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.requests, request)
		m.scheduleLocked()
		m.mu.Unlock()
		cancel(nil)
	}()
	// Admission never waits with a source transaction open. A second transaction
	// captures the actual barrier; growth between these reads is checked below.
	var reservation int64
	estimateErr := read(captureCtx, func(tx *sql.Tx, _ Sequence) error {
		var err error
		reservation, err = estimateSpill(captureCtx, tx, scope)
		return err
	})
	m.mu.Lock()
	m.expireLocked()
	estimateElapsed := m.now().Sub(request.started)
	request.estimating = false
	m.scheduleLocked()
	m.mu.Unlock()
	if errors.Is(context.Cause(captureCtx), errSpillUnavailable) {
		return nil, errSpillUnavailable
	}
	if estimateErr != nil {
		return nil, estimateErr
	}
	s, err := m.acquire(captureCtx, reservation, scope)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	s.cancel = cancel
	s.progressAt = s.progressAt.Add(-estimateElapsed)
	m.scheduleLocked()
	removed := s.removed
	m.mu.Unlock()
	if removed {
		cancel(errSpillExpired)
	}
	success := false
	defer func() {
		for _, f := range []*os.File{s.data, s.records, s.index} {
			if f != nil {
				_ = f.Close()
				m.mu.Lock()
				delete(s.handles, f)
				m.mu.Unlock()
			}
		}
		if !success {
			m.mu.Lock()
			m.removeLocked(s, errSpillUnavailable)
			m.mu.Unlock()
			_ = os.RemoveAll(s.dir)
		}
	}()
	if err := os.Mkdir(s.dir, 0700); err != nil {
		return nil, fmt.Errorf("%w: %v", errSpillUnavailable, err)
	}
	for _, f := range []struct {
		name   string
		target **os.File
	}{{"data", &s.data}, {"records", &s.records}, {"parts", &s.index}} {
		*f.target, err = os.OpenFile(filepath.Join(s.dir, f.name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errSpillUnavailable, err)
		}
		m.mu.Lock()
		if s.removed {
			m.mu.Unlock()
			return nil, errSpillUnavailable
		}
		s.handles[*f.target] = true
		m.mu.Unlock()
	}
	var out io.Writer = s.data
	if m.wrapWriter != nil {
		out = m.wrapWriter(out)
	}
	c := &spillCapture{s: s, writer: captureWriter{s: s, out: out}, root: -1}
	defer func() {
		if c.refs != nil {
			_ = c.refs.Close()
			m.mu.Lock()
			delete(s.handles, c.refs)
			m.mu.Unlock()
		}
	}()
	err = read(captureCtx, func(tx *sql.Tx, seq Sequence) error {
		s.Sequence = seq
		return c.capture(captureCtx, tx, scope, order)
	})
	if err == nil {
		err = c.finish(captureCtx)
	}
	if cause := context.Cause(captureCtx); cause != nil {
		err = cause
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSpillUnavailable, err)
	}
	var bytes int64
	for _, f := range []*os.File{s.data, s.records, s.index} {
		info, e := f.Stat()
		if e != nil {
			return nil, e
		}
		bytes += info.Size()
	}
	if c.refs != nil {
		info, e := c.refs.Stat()
		if e != nil {
			return nil, e
		}
		bytes += info.Size()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, errSpillUnavailable
	}
	if !s.over {
		if bytes > s.reserved && bytes-s.reserved > m.budget-m.used {
			if m.over != nil || m.overGrantedLocked() || m.waitingLocked() {
				return nil, errSpillUnavailable
			}
			m.used -= s.reserved
			s.over = true
			m.over = s
		} else {
			m.used += bytes - s.reserved
		}
	}
	if s.over && bytes <= m.budget-m.used {
		s.over = false
		m.over = nil
		m.used += bytes
		m.admitLocked()
	}
	s.reserved = bytes
	m.admitLocked()
	s.capturing = false
	s.ended = m.now()
	s.last = s.ended
	m.contentionLocked()
	success = true
	return s, nil
}

func validateSpillScope(s Scope) error {
	n := 0
	if s.Tree {
		n++
	}
	if s.Page != "" {
		n++
		if err := s.Page.ValidateKind(PageKind); err != nil {
			return err
		}
	}
	if s.Database != "" {
		n++
		if err := s.Database.ValidateKind(DatabaseKind); err != nil {
			return err
		}
	}
	if n != 1 || s.View != nil && s.Database == "" {
		return fmt.Errorf("invalid snapshot scope")
	}
	return nil
}

func scopeIDs(s Scope) (string, []any) {
	if s.Tree {
		return `SELECT p.id FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1 ORDER BY p.parent,p.position,p.id`, nil
	}
	if s.Page != "" {
		return `SELECT id FROM items WHERE alive=1 AND (id=? OR id IN (SELECT id FROM blocks WHERE page=?) OR id IN (SELECT d.id FROM databases d JOIN blocks b ON b.id=d.block WHERE b.page=?) OR id IN (SELECT p.id FROM properties p JOIN databases d ON d.id=p.database_id JOIN blocks b ON b.id=d.block WHERE b.page=?) OR id IN (SELECT o.id FROM options o JOIN properties p ON p.id=o.property_id JOIN databases d ON d.id=p.database_id JOIN blocks b ON b.id=d.block WHERE b.page=?) OR id IN (SELECT v.id FROM views v JOIN databases d ON d.id=v.database_id JOIN blocks b ON b.id=d.block WHERE b.page=?)) ORDER BY kind,id`, []any{s.Page, s.Page, s.Page, s.Page, s.Page, s.Page}
	}
	return `SELECT id FROM items WHERE alive=1 AND (id=? OR id IN (SELECT id FROM properties WHERE database_id=?) OR id IN (SELECT o.id FROM options o JOIN properties p ON p.id=o.property_id WHERE p.database_id=?) OR id IN (SELECT id FROM views WHERE database_id=?) OR id IN (SELECT id FROM rows WHERE database_id=?)) ORDER BY kind,id`, []any{s.Database, s.Database, s.Database, s.Database, s.Database}
}

func estimateSpill(ctx context.Context, tx *sql.Tx, scope Scope) (int64, error) {
	query, args := scopeIDs(scope)
	// Stored JSON already includes UTF-8 escaping. Reserve content plus stamps
	// and disk-index overhead; charge checks actual growth before every write.
	query = `WITH selected AS (` + query + `) SELECT COALESCE(sum(length(CAST(i.data AS BLOB))+256),0) + COALESCE((SELECT sum(length(CAST(c.value AS BLOB))+length(c.property_id)+8) FROM cells c JOIN selected s ON s.id=c.row_id),0) + COALESCE((SELECT sum(length(CAST(o.ids AS BLOB))+32) FROM orders o JOIN selected s ON s.id=o.owner_id),0) + COALESCE((SELECT sum(length(g.group_key)+length(g.property_id)+length(g.version)+24) FROM group_versions g JOIN selected s ON s.id=g.item_id),0) + 48 FROM items i JOIN selected s ON s.id=i.id`
	var bytes int64
	err := tx.QueryRowContext(ctx, query, args...).Scan(&bytes)
	return bytes, err
}

type spillCapture struct {
	s                                  *Spill
	writer                             captureWriter
	nodes, root, schemaNodes, rowCount int64
	schema                             Schema
	view                               ViewDef
	order                              func(Schema, ViewDef, []RowValues) []ID
	refs                               *os.File
	refSlots                           int64
	queryError                         error
	callback                           bool
	ranks                              *os.File
	rankSlots                          int64
}

func (c *spillCapture) capture(ctx context.Context, tx *sql.Tx, scope Scope, order func(Schema, ViewDef, []RowValues) []ID) error {
	c.order = order
	if scope.View != nil {
		c.view = *scope.View
	}
	if order != nil && reflect.ValueOf(order).Pointer() != reflect.ValueOf(OrderRows).Pointer() && scope.Database != "" {
		if err := c.callbackSchema(ctx, tx, scope.Database); err != nil {
			return err
		}
	}
	if !scope.Public {
		if err := c.prepareReferences(ctx, tx, scope); err != nil {
			return err
		}
	}
	if scope.Database != "" {
		return c.captureDatabase(ctx, tx, scope)
	}

	query, args := scopeIDs(scope)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var id ID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if err := c.addRecord(ctx, tx, id, false); err != nil {
			return err
		}
	}
	c.schemaNodes = c.nodes
	return rows.Err()
}

func writeSpillRow(w io.Writer, item Item, public bool) error {
	row := item.Data.(Row)
	if item.Kind() != RowKind {
		return fmt.Errorf("row data does not match id kind")
	}
	if !public {
		if err := item.Validate(); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	field := func(key string, value any) error {
		if err := enc.Encode(key); err != nil {
			return err
		}
		if _, err := io.WriteString(w, ":"); err != nil {
			return err
		}
		if err := enc.Encode(value); err != nil {
			return err
		}
		_, err := io.WriteString(w, ",")
		return err
	}
	if public {
		if err := field("id", item.ID()); err != nil {
			return err
		}
	} else {
		if err := field("ref", item.Ref); err != nil {
			return err
		}
		if err := field("ver", item.Ver); err != nil {
			return err
		}
		if err := field("v", item.V); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		key   string
		value any
	}{{"kind", RowKind}, {"database", row.Database}, {"position", row.Position}} {
		if err := field(f.key, f.value); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\"cells\":{"); err != nil {
		return err
	}
	first := true
	ids := make([]ID, 0, len(row.Cells))
	for id := range row.Cells {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		value := row.Cells[id]
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		if err := enc.Encode(id); err != nil {
			return err
		}
		if _, err := io.WriteString(w, ":"); err != nil {
			return err
		}
		if err := writeSpillCell(w, value); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}}\n")
	return err
}

func writeSpillCell(w io.Writer, value CellValue) error {
	value, err := value.Canonical()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, `{"type":`); err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(value.Type); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"value":`); err != nil {
		return err
	}
	switch v := value.Value.(type) {
	case string:
		err = writeSpillString(w, v)
	case Decimal:
		err = writeSpillString(w, string(v))
	case CalendarDate:
		err = writeSpillString(w, string(v))
	case []ID:
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for n, id := range v {
			if n > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := json.NewEncoder(w).Encode(id); err != nil {
				return err
			}
		}
		_, err = io.WriteString(w, "]")
	default:
		err = json.NewEncoder(w).Encode(v)
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "}")
	return err
}

func writeSpillString(w io.Writer, text string) error {
	if _, err := io.WriteString(w, `"`); err != nil {
		return err
	}
	for len(text) > 0 {
		n := min(len(text), spillChunk/4)
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		encoded, err := json.Marshal(text[:n])
		if err != nil {
			return err
		}
		if _, err := w.Write(encoded[1 : len(encoded)-1]); err != nil {
			return err
		}
		text = text[n:]
	}
	_, err := io.WriteString(w, `"`)
	return err
}

func rowValues(i Item) RowValues {
	r := i.Data.(Row)
	return RowValues{Row: i.ID(), Position: r.Position, Cells: r.Cells}
}

func (c *spillCapture) addRecord(ctx context.Context, tx *sql.Tx, id ID, row bool) error {
	item := Item{Ref: Ref{ID: id}}
	if row && c.s.scope.View != nil {
		values := Row{Cells: map[ID]CellValue{}}
		if err := tx.QueryRowContext(ctx, `SELECT database_id,position FROM rows WHERE id=?`, id).Scan(&values.Database, &values.Position); err != nil {
			return err
		}
		include := func(target Target) error {
			if target.Ref == nil {
				return nil
			}
			property := target.Ref.ID
			if _, ok := values.Cells[property]; ok {
				return nil
			}
			raw := func() io.Reader {
				return &spillColumnReader{ctx: ctx, q: tx, query: `SELECT substr(CAST(value AS BLOB),?,?) FROM cells WHERE row_id=? AND property_id=?`, args: []any{id, property}}
			}
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cells WHERE row_id=? AND property_id=?)`, id, property).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return nil
			}
			var typ PropertyType
			if err := readRecordField(raw(), "type", &typ); err != nil {
				return err
			}
			var size int64
			if err := tx.QueryRowContext(ctx, `SELECT length(CAST(value AS BLOB)) FROM cells WHERE row_id=? AND property_id=?`, id, property).Scan(&size); err != nil {
				return err
			}
			if !c.callback && typ == MultiSelectType {
				values.Cells[property] = CellValue{Type: typ, Value: &spillQueryCollection{raw: raw, capture: c}}
				return nil
			}
			if !c.callback && size > spillChunk && (typ == TitleType || typ == TextType || typ == URLType) {
				values.Cells[property] = CellValue{Type: typ, Value: &spillQueryText{raw: raw, err: &c.queryError}}
				return nil
			}
			var value CellValue
			if err := json.NewDecoder(raw()).Decode(&value); err != nil {
				return err
			}
			values.Cells[property] = value
			return nil
		}
		for _, sort := range c.view.Sort {
			if err := include(sort.Property); err != nil {
				return err
			}
		}
		for _, filter := range c.view.Filter {
			if err := include(filter.Property); err != nil {
				return err
			}
		}
		item.Data = values
	}
	return c.addEncoded(item, row, func(w io.Writer) error {
		return loadSpillRecord(ctx, tx, id, w, c.s.scope.Public)
	})
}

func (c *spillCapture) add(item Item, row bool) error {
	return c.addEncoded(item, row, func(w io.Writer) error {
		if c.s.scope.Public {
			return writePublicItem(w, item)
		}
		if _, ok := item.Data.(Row); ok {
			return writeSpillRow(w, item, false)
		}
		return json.NewEncoder(w).Encode(item)
	})
}

func (c *spillCapture) addEncoded(item Item, row bool, encode func(io.Writer) error) error {
	if row && c.s.scope.View != nil {
		if c.order == nil {
			return fmt.Errorf("database view requires row ordering")
		}
		ids := c.order(c.schema, c.view, []RowValues{rowValues(item)})
		if c.queryError != nil {
			return c.queryError
		}
		if len(ids) == 0 {
			return nil
		}
	}
	start := c.writer.offset
	err := encode(&c.writer)
	if err != nil {
		return err
	}
	n := spillNode{start: start, length: c.writer.offset - start, height: 1, left: -1, right: -1, rows: 1}
	if !row {
		if err := c.indexReference(n); err != nil {
			return err
		}
	}
	n.bytes = n.length + 1
	if !row || c.s.scope.View == nil {
		previous := c.nodes - 1
		if row && c.rowCount == 0 {
			previous = -1
		}
		if previous >= 0 {
			p, err := c.node(previous)
			if err != nil {
				return err
			}
			n.bytes += p.bytes
		}
	}
	if row && c.s.scope.View != nil {
		n.keyStart, n.keyLength = n.start, n.length
	}
	at := c.nodes
	if err := c.s.charge(spillNodeSize); err != nil {
		return err
	}
	c.nodes++
	if err := c.putNode(at, n); err != nil {
		return err
	}
	if row {
		c.rowCount++
		if c.s.scope.View != nil {
			c.root, err = c.insert(c.root, at)
		}
	}
	return err
}

func (c *spillCapture) node(at int64) (spillNode, error) {
	var b [spillNodeSize]byte
	if _, err := c.s.records.ReadAt(b[:], at*spillNodeSize); err != nil {
		return spillNode{}, err
	}
	a := [9]int64{}
	for i := range a {
		a[i] = int64(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return spillNode{a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7], a[8]}, nil
}
func (c *spillCapture) putNode(at int64, n spillNode) error {
	var b [spillNodeSize]byte
	for i, v := range []int64{n.start, n.length, n.left, n.right, n.height, n.rows, n.bytes, n.keyStart, n.keyLength} {
		binary.LittleEndian.PutUint64(b[i*8:], uint64(v))
	}
	_, err := c.s.records.WriteAt(b[:], at*spillNodeSize)
	return err
}
func (c *spillCapture) row(at int64) (RowValues, error) {
	n, err := c.node(at)
	if err != nil {
		return RowValues{}, err
	}
	r := RowValues{Cells: map[ID]CellValue{}}
	err = newReadJSONRanges(c.s.data, n.keyStart, n.keyLength).each(true, func(name string, start, end int64) error {
		switch name {
		case "id":
			return readSmallRange(c.s.data, start, end, &r.Row)
		case "ref":
			var ref Ref
			if err := readSmallRange(c.s.data, start, end, &ref); err != nil {
				return err
			}
			r.Row = ref.ID
			return nil
		case "position":
			return readSmallRange(c.s.data, start, end, &r.Position)
		case "cells":
			return newReadJSONRanges(c.s.data, start, end-start).each(true, func(name string, start, end int64) error {
				wanted := false
				for _, key := range c.view.Sort {
					wanted = wanted || key.Property.Ref != nil && key.Property.Ref.ID == ID(name)
				}
				for _, key := range c.view.Filter {
					wanted = wanted || key.Property.Ref != nil && key.Property.Ref.ID == ID(name)
				}
				if !wanted {
					return nil
				}
				raw := func() io.Reader { return io.NewSectionReader(c.s.data, start, end-start) }
				var typ PropertyType
				if err := readRecordField(raw(), "type", &typ); err != nil {
					return err
				}
				if !c.callback && typ == MultiSelectType {
					r.Cells[ID(name)] = CellValue{Type: typ, Value: &spillQueryCollection{raw: raw, capture: c}}
					return nil
				}
				if !c.callback && end-start > spillChunk && (typ == TitleType || typ == TextType || typ == URLType) {
					r.Cells[ID(name)] = CellValue{Type: typ, Value: &spillQueryText{raw: raw, err: &c.queryError}}
					return nil
				}
				var value CellValue
				if err := json.NewDecoder(raw()).Decode(&value); err != nil {
					return err
				}
				r.Cells[ID(name)] = value
				return nil
			})
		}
		return nil
	})
	return r, err
}
func (c *spillCapture) less(a, b int64) (bool, error) {
	x, err := c.row(a)
	if err != nil {
		return false, err
	}
	y, err := c.row(b)
	if err != nil {
		return false, err
	}
	if c.s.scope.View != nil {
		pair := []RowValues{x, y}
		if y.Position < x.Position || y.Position == x.Position && y.Row < x.Row {
			pair[0], pair[1] = pair[1], pair[0]
		}
		ids := c.order(c.schema, c.view, pair)
		if c.queryError != nil {
			return false, c.queryError
		}
		if len(ids) != 2 {
			return false, fmt.Errorf("row ordering changed membership")
		}
		return ids[0] == x.Row, nil
	}
	if x.Position != y.Position {
		return x.Position < y.Position, nil
	}
	return x.Row < y.Row, nil
}
func (c *spillCapture) height(at int64) (int64, error) {
	if at < 0 {
		return 0, nil
	}
	n, err := c.node(at)
	return n.height, err
}
func (c *spillCapture) update(at int64, n spillNode) error {
	l, err := c.height(n.left)
	if err != nil {
		return err
	}
	r, err := c.height(n.right)
	if err != nil {
		return err
	}
	n.height = 1 + max(l, r)
	left, err := c.summary(n.left)
	if err != nil {
		return err
	}
	right, err := c.summary(n.right)
	if err != nil {
		return err
	}
	n.rows = left.rows + right.rows + 1
	n.bytes = left.bytes + right.bytes + n.length + 1
	return c.putNode(at, n)
}
func (c *spillCapture) rotate(at int64, left bool) (int64, error) {
	n, err := c.node(at)
	if err != nil {
		return 0, err
	}
	child := n.left
	if left {
		child = n.right
	}
	v, err := c.node(child)
	if err != nil {
		return 0, err
	}
	if left {
		n.right = v.left
		v.left = at
	} else {
		n.left = v.right
		v.right = at
	}
	if err := c.update(at, n); err != nil {
		return 0, err
	}
	return child, c.update(child, v)
}
func (c *spillCapture) insert(root, at int64) (int64, error) {
	if root < 0 {
		return at, nil
	}
	n, err := c.node(root)
	if err != nil {
		return 0, err
	}
	less, err := c.less(at, root)
	if err != nil {
		return 0, err
	}
	if less {
		n.left, err = c.insert(n.left, at)
	} else {
		n.right, err = c.insert(n.right, at)
	}
	if err != nil {
		return 0, err
	}
	if err := c.update(root, n); err != nil {
		return 0, err
	}
	l, err := c.height(n.left)
	if err != nil {
		return 0, err
	}
	r, err := c.height(n.right)
	if err != nil {
		return 0, err
	}
	if l-r > 1 {
		v, err := c.node(n.left)
		if err != nil {
			return 0, err
		}
		a, err := c.height(v.left)
		if err != nil {
			return 0, err
		}
		b, err := c.height(v.right)
		if err != nil {
			return 0, err
		}
		if b > a {
			n.left, err = c.rotate(n.left, true)
			if err != nil {
				return 0, err
			}
			if err := c.putNode(root, n); err != nil {
				return 0, err
			}
		}
		return c.rotate(root, false)
	}
	if r-l > 1 {
		v, err := c.node(n.right)
		if err != nil {
			return 0, err
		}
		a, err := c.height(v.left)
		if err != nil {
			return 0, err
		}
		b, err := c.height(v.right)
		if err != nil {
			return 0, err
		}
		if a > b {
			n.right, err = c.rotate(n.right, false)
			if err != nil {
				return 0, err
			}
			if err := c.putNode(root, n); err != nil {
				return 0, err
			}
		}
		return c.rotate(root, true)
	}
	return root, nil
}

func (c *spillCapture) prefix(rows bool, n int64) (int64, error) {
	if n == 0 {
		return 0, nil
	}
	if !rows || c.s.scope.View == nil {
		at := n - 1
		if rows {
			at += c.schemaNodes
		}
		node, err := c.node(at)
		if err != nil {
			return 0, err
		}
		return node.bytes, nil
	}
	at := c.root
	var sum int64
	for at >= 0 && n > 0 {
		node, err := c.node(at)
		if err != nil {
			return 0, err
		}
		left, err := c.summary(node.left)
		if err != nil {
			return 0, err
		}
		if n <= left.rows {
			at = node.left
			continue
		}
		sum += left.bytes + node.length + 1
		n -= left.rows + 1
		at = node.right
	}
	return sum, nil
}

func (c *spillCapture) summary(at int64) (spillNode, error) {
	if at < 0 {
		return spillNode{}, nil
	}
	return c.node(at)
}
func (c *spillCapture) at(rows bool, n int64) (spillNode, error) {
	if !rows {
		return c.node(n)
	}
	if c.s.scope.View == nil {
		return c.node(c.schemaNodes + n)
	}
	at := c.root
	for at >= 0 {
		node, err := c.node(at)
		if err != nil {
			return spillNode{}, err
		}
		left, err := c.summary(node.left)
		if err != nil {
			return spillNode{}, err
		}
		if n < left.rows {
			at = node.left
		} else if n == left.rows {
			return node, nil
		} else {
			n -= left.rows + 1
			at = node.right
		}
	}
	return spillNode{}, fmt.Errorf("invalid snapshot row offset")
}

// Window descriptors locate content through prefix sums in the on-disk tree.
// Finishing does not scan or re-encode the captured records.
func (c *spillCapture) finish(ctx context.Context) error {
	c.s.root = c.root
	c.s.schemaNodes = c.schemaNodes
	for window, start := int64(0), int64(0); ; window++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		w := spillWindow{first: int64(c.s.parts), start: start, count: c.schemaNodes}
		rows := window > 0
		if rows {
			w.count = min(int64(1000), c.rowCount-start)
		}
		before, err := c.prefix(rows, start)
		if err != nil {
			return err
		}
		after, err := c.prefix(rows, start+w.count)
		if err != nil {
			return err
		}
		w.bytes = int64(len(spillWindowPrefix) + 2)
		if w.count > 0 {
			w.bytes += after - before - 1
		}
		w.parts = (w.bytes + spillChunk - 1) / spillChunk
		if !rows && c.rowCount == 0 || rows && start+w.count == c.rowCount {
			w.final = 1
		}
		if err := c.s.charge(spillWindowSize); err != nil {
			return err
		}
		if err := c.s.putWindow(window, w); err != nil {
			return err
		}
		c.s.parts += int(w.parts)
		c.s.windows++
		if w.final == 1 {
			return nil
		}
		if rows {
			start += w.count
		}
	}
}

const spillWindowPrefix = `{"items":[`
const spillWindowSize = 48

type spillWindow struct{ first, parts, start, count, bytes, final int64 }

func (s *Spill) putWindow(at int64, w spillWindow) error {
	var b [spillWindowSize]byte
	for i, v := range []int64{w.first, w.parts, w.start, w.count, w.bytes, w.final} {
		binary.LittleEndian.PutUint64(b[i*8:], uint64(v))
	}
	_, err := s.index.WriteAt(b[:], at*spillWindowSize)
	return err
}
func readSpillWindow(f *os.File, at int64) (spillWindow, error) {
	var b [spillWindowSize]byte
	if _, err := f.ReadAt(b[:], at*spillWindowSize); err != nil {
		return spillWindow{}, err
	}
	a := [6]int64{}
	for i := range a {
		a[i] = int64(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return spillWindow{a[0], a[1], a[2], a[3], a[4], a[5]}, nil
}

func (c *spillCapture) rangeData(w spillWindow, rows bool, start, end int64) ([]byte, error) {
	result := make([]byte, 0, end-start)
	appendBytes := func(data []byte, at int64) {
		lo, hi := max(start-at, int64(0)), min(end-at, int64(len(data)))
		if lo < hi {
			result = append(result, data[lo:hi]...)
		}
	}
	appendBytes([]byte(spillWindowPrefix), 0)
	before, err := c.prefix(rows, w.start)
	if err != nil {
		return nil, err
	}
	// Seek directly to the first intersecting record, rather than scanning a window.
	lo, hi := int64(0), w.count
	for lo < hi {
		mid := (lo + hi) / 2
		p, err := c.prefix(rows, w.start+mid+1)
		if err != nil {
			return nil, err
		}
		if int64(len(spillWindowPrefix))+p-before <= start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	for j := lo; j < w.count; j++ {
		prefix, err := c.prefix(rows, w.start+j)
		if err != nil {
			return nil, err
		}
		at := int64(len(spillWindowPrefix)) + prefix - before
		if at >= end {
			break
		}
		node, err := c.at(rows, w.start+j)
		if err != nil {
			return nil, err
		}
		from, to := max(start-at, int64(0)), min(end-at, node.length)
		if from < to {
			n := len(result)
			result = append(result, make([]byte, to-from)...)
			if _, err := c.s.data.ReadAt(result[n:], node.start+from); err != nil {
				return nil, err
			}
		}
		if j+1 < w.count {
			appendBytes([]byte(","), at+node.length)
		}
	}
	appendBytes([]byte("]}"), w.bytes-2)
	return result, nil
}
func (c *spillCapture) boundary(w spillWindow, rows bool, at int64) (int64, error) {
	if at == 0 || at >= w.bytes {
		return at, nil
	}
	for {
		data, err := c.rangeData(w, rows, at, at+1)
		if err != nil {
			return 0, err
		}
		if len(data) != 1 {
			return 0, io.ErrUnexpectedEOF
		}
		if utf8.RuneStart(data[0]) {
			return at, nil
		}
		at--
	}
}

// Part returns one bounded encoded continuation. Replay renews only fitting spills.
func (s *Spill) Part(i int) (result []byte, total int, resultErr error) {
	m := s.manager
	ioStarted := false
	defer func() {
		if resultErr == nil || !ioStarted {
			return
		}
		m.mu.Lock()
		removed := s.removed
		m.mu.Unlock()
		if removed {
			resultErr = errSpillExpired
		} else if !errors.Is(resultErr, errSpillUnavailable) {
			resultErr = fmt.Errorf("%w: %v", errSpillUnavailable, resultErr)
		}
	}()
	m.mu.Lock()
	m.expireLocked()
	if s.removed {
		m.mu.Unlock()
		return nil, 0, errSpillExpired
	}
	if s.capturing || i < 0 || i >= s.parts {
		m.mu.Unlock()
		return nil, s.parts, fmt.Errorf("invalid snapshot part")
	}
	// Open under the lifecycle lock, then do bounded reads without blocking other readers.
	var files [3]*os.File
	for j, name := range []string{"data", "records", "parts"} {
		f, err := os.Open(filepath.Join(s.dir, name))
		if err != nil {
			for _, opened := range files {
				if opened != nil {
					_ = opened.Close()
					delete(s.handles, opened)
				}
			}
			m.mu.Unlock()
			return nil, s.parts, fmt.Errorf("%w: %v", errSpillUnavailable, err)
		}
		files[j] = f
		s.handles[f] = true
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, f := range files {
			_ = f.Close()
			delete(s.handles, f)
		}
	}()
	ioStarted = true
	local := &Spill{data: files[0], records: files[1], index: files[2], scope: s.scope}
	c := &spillCapture{s: local, root: s.root, schemaNodes: s.schemaNodes}
	lo, hi := int64(0), s.windows
	var w spillWindow
	for lo < hi {
		mid := (lo + hi) / 2
		v, err := readSpillWindow(local.index, mid)
		if err != nil {
			return nil, s.parts, fmt.Errorf("%w: %v", errSpillUnavailable, err)
		}
		if v.first+v.parts <= int64(i) {
			lo = mid + 1
		} else {
			hi = mid
			w = v
		}
	}
	window := lo
	if w.parts == 0 || int64(i) < w.first {
		return nil, s.parts, errSpillUnavailable
	}
	part := int64(i) - w.first
	start, err := c.boundary(w, window > 0, part*spillChunk)
	if err != nil {
		return nil, s.parts, err
	}
	end, err := c.boundary(w, window > 0, min((part+1)*spillChunk, w.bytes))
	if err != nil {
		return nil, s.parts, err
	}
	data, err := c.rangeData(w, window > 0, start, end)
	if err != nil {
		return nil, s.parts, fmt.Errorf("%w: %v", errSpillUnavailable, err)
	}
	rows := int64(0)
	if window > 0 {
		rows = w.count
	}
	out, err := json.Marshal(struct {
		Seq       Sequence `json:"seq"`
		Window    int64    `json:"window"`
		Rows      int64    `json:"rows"`
		Final     bool     `json:"final_window"`
		SpillPart int      `json:"spill_part"`
		Part      int64    `json:"part"`
		Parts     int64    `json:"parts"`
		Data      string   `json:"data"`
	}{s.Sequence, window, rows, w.final == 1, i, part, w.parts, string(data)})
	if err != nil {
		return nil, s.parts, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, s.parts, errSpillExpired
	}
	if i > s.high {
		s.high = i
		s.last = m.now()
	}
	if !s.over {
		s.last = m.now()
	}
	if i == s.parts-1 {
		s.traversed = true
	}
	if s.over && s.traversed && m.waitingLocked() {
		m.removeLocked(s, errSpillExpired)
	}
	return out, s.parts, nil
}
