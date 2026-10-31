package notebook

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
)

func readBoundedField(source io.Reader, field string, out any) error {
	r, err := spillFieldReader(source, field)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(io.LimitReader(r, spillChunk+1))
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.InputOffset() > spillChunk {
		return fmt.Errorf("oversized query metadata")
	}
	return nil
}

func readStoredQuery(source func() io.Reader) (StoredView, error) {
	v := StoredView{Name: "Query", Display: StoredDisplay{Layout: "table", Visible: []ID{}, Widths: []StoredWidth{}}}
	for _, field := range []string{"sort", "filter"} {
		definition, err := spillFieldReader(source(), "definition")
		if err != nil {
			return v, err
		}
		var out any = &v.Sort
		if field == "filter" {
			out = &v.Filter
		}
		if err := readBoundedField(definition, field, out); err != nil {
			return v, err
		}
	}
	return v, nil
}

const spillRankSlot = 40

func (c *spillCapture) prepareQueryRanks(ctx context.Context, tx *sql.Tx, database ID) error {
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM options o JOIN properties p ON p.id=o.property_id WHERE p.database_id=?`, database).Scan(&count); err != nil {
		return err
	}
	c.rankSlots = 1
	for c.rankSlots < count*2 {
		c.rankSlots *= 2
	}
	f, err := c.queryFile("query-ranks")
	if err != nil {
		return err
	}
	c.ranks = f
	if err := c.s.charge(c.rankSlots * spillRankSlot); err != nil {
		return err
	}
	return f.Truncate(c.rankSlots * spillRankSlot)
}

func (c *spillCapture) queryFile(name string) (*os.File, error) {
	f, err := os.CreateTemp(c.s.dir, name)
	if err != nil {
		return nil, err
	}
	c.s.manager.mu.Lock()
	defer c.s.manager.mu.Unlock()
	if c.s.removed {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, errSpillUnavailable
	}
	c.s.handles[f] = true
	return f, nil
}

func (c *spillCapture) closeQueryFile(f *os.File) {
	_ = f.Close()
	c.s.manager.mu.Lock()
	delete(c.s.handles, f)
	c.s.manager.mu.Unlock()
	_ = os.Remove(f.Name())
}

func (c *spillCapture) queryRank(id ID, rank *int) (int, error) {
	hash := sha256.Sum256([]byte(id))
	at := int64(binary.LittleEndian.Uint64(hash[:8]) & uint64(c.rankSlots-1))
	var slot [spillRankSlot]byte
	for probes := int64(0); probes < c.rankSlots; probes++ {
		if _, err := c.ranks.ReadAt(slot[:], at*spillRankSlot); err != nil {
			return 0, err
		}
		if slot[0] == 0 {
			if rank == nil {
				return 0, nil
			}
			slot[0] = byte(len(id))
			copy(slot[1:30], id)
			binary.LittleEndian.PutUint64(slot[32:], uint64(*rank))
			_, err := c.ranks.WriteAt(slot[:], at*spillRankSlot)
			return *rank, err
		}
		if string(slot[1:1+int(slot[0])]) == string(id) {
			return int(binary.LittleEndian.Uint64(slot[32:])), nil
		}
		at = (at + 1) & (c.rankSlots - 1)
	}
	return 0, fmt.Errorf("snapshot option index full")
}

type spillQueryCollection struct {
	raw     func() io.Reader
	capture *spillCapture
}

func (v *spillQueryCollection) fail(err error) {
	if err != nil && v.capture.queryError == nil {
		v.capture.queryError = err
	}
}

func (v *spillQueryCollection) each(visit func(ID) bool) {
	r, err := spillFieldReader(v.raw(), "value")
	if err != nil {
		v.fail(err)
		return
	}
	dec := json.NewDecoder(r)
	token, err := dec.Token()
	if err != nil {
		v.fail(err)
		return
	}
	if token == nil {
		return
	}
	if token != json.Delim('[') {
		v.fail(fmt.Errorf("invalid query collection"))
		return
	}
	for dec.More() {
		var id ID
		if err := dec.Decode(&id); err != nil {
			v.fail(err)
			return
		}
		if !visit(id) {
			return
		}
	}
	_, err = dec.Token()
	v.fail(err)
}

func (v *spillQueryCollection) contains(id ID) bool {
	found := false
	v.each(func(value ID) bool { found = value == id; return !found })
	return found
}

// A rank bitset deduplicates and orders retained collections on disk. Its size
// depends on schema option order, not the number or byte size of selected ids.
func (v *spillQueryCollection) ranks(column dbQueryColumn) (func() (int, bool), func()) {
	f, err := v.capture.queryFile("query-set")
	if err != nil {
		v.fail(err)
		return func() (int, bool) { return 0, false }, func() {}
	}
	var size int64
	previous := -1
	v.each(func(id ID) bool {
		rank := column.optionPosition(id)
		if rank == previous {
			return true
		}
		previous = rank
		at := int64(rank / 8)
		if at >= size {
			if err := v.capture.s.charge(at + 1 - size); err != nil {
				v.fail(err)
				return false
			}
			size = at + 1
		}
		var b [1]byte
		_, err := f.ReadAt(b[:], at)
		if err != nil && !errors.Is(err, io.EOF) {
			v.fail(err)
			return false
		}
		mask := byte(1 << (rank % 8))
		if b[0]&mask != 0 {
			return true
		}
		b[0] |= mask
		_, err = f.WriteAt(b[:], at)
		v.fail(err)
		return err == nil
	})
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, size), spillChunk)
	at := -1
	var current byte
	next := func() (int, bool) {
		for current == 0 {
			b, err := r.ReadByte()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					v.fail(err)
				}
				return 0, false
			}
			at++
			current = b
		}
		bit := bits.TrailingZeros8(current)
		current &= current - 1
		return at*8 + bit, true
	}
	return next, func() {
		v.capture.closeQueryFile(f)
		v.fail(v.capture.s.charge(-size))
	}
}

func queryRankIterator(column dbQueryColumn, value dbQueryValue) (func() (int, bool), func()) {
	if value.collection != nil {
		return value.collection.ranks(column)
	}
	at := 0
	return func() (int, bool) {
		if at == len(value.ranks) {
			return 0, false
		}
		rank := value.ranks[at]
		at++
		return rank, true
	}, func() {}
}

func queryCollectionCompare(column dbQueryColumn, a, b dbQueryValue) int {
	x, closeX := queryRankIterator(column, a)
	defer closeX()
	y, closeY := queryRankIterator(column, b)
	defer closeY()
	for {
		av, ap := x()
		bv, bp := y()
		if !ap || !bp {
			if ap {
				return 1
			}
			if bp {
				return -1
			}
			return 0
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
}
