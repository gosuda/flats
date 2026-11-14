package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// auditPage is how many events `flats audit ls` reads in one read transaction (rule 22 of docs/spec/audit-events.md).
const auditPage = 500

// defaultAuditLimit is how many of the newest events `flats audit ls` lists without --limit.
const defaultAuditLimit = 100

// AuditCommand runs `flats audit ls`. It works on the data directory, reads flats.db without a host lock and never
// calls the host.
func AuditCommand(ctx context.Context, args []string, out io.Writer) error {
	c := &auditCmd{configCmd: configCmd{ctx: ctx, out: out}}
	if len(args) == 0 || args[0] != "ls" {
		return usagef("usage: flats audit ls [--since TIME] [--kind KIND] [--flat SLUG] [--flat-id ID] [--limit N] [--json] [--config PATH | --data DIR]")
	}
	f := c.flags("audit ls")
	since := f.String("since", "", "only events from this RFC 3339 time, or from this long ago (a duration such as 24h)")
	kind := f.String("kind", "", "only events of this kind")
	flat := f.String("flat", "", "only events of the flat that has this slug (or, once it is gone, that was written under it)")
	flatID := f.String("flat-id", "", "only events of the flat with this ID")
	limit := f.Int("limit", defaultAuditLimit, "list the newest N matching events; 0 lists all")
	asJSON := f.Bool("json", false, "print one JSON object per line")
	if err := c.parseFlags(args[1:]); err != nil {
		return err
	}
	if *limit < 0 {
		return usagef("--limit must be 0 or more")
	}
	if *flat != "" && *flatID != "" {
		return usagef("use --flat or --flat-id, not both")
	}
	q := store.AuditQuery{Kind: *kind, Flat: *flat, FlatID: *flatID}
	if *since != "" {
		t, err := parseSince(*since, time.Now())
		if err != nil {
			return usagef("--since: %v", err)
		}
		q.Since = t
	}
	dataDir, _, err := c.location()
	if err != nil {
		return err
	}
	rd, err := store.OpenAuditReader(filepath.Join(dataDir, "flats.db"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return actionf("%s has no flats.db: it is not the data directory of a Flats host", dataDir)
	case errors.Is(err, store.ErrSchemaTooNew):
		return actionf("this Flats is older than the database in %s; use the release that wrote it", dataDir)
	case errors.Is(err, store.ErrAuditSchema):
		return actionf("the database in %s is of an older schema than this Flats expects: start the host once with this Flats so that it migrates it", dataDir)
	case err != nil:
		return err
	}
	defer rd.Close()
	if *flat != "" {
		id, found, err := rd.FlatID(ctx, *flat)
		if err != nil {
			return err
		}
		if found {
			q.Flat, q.FlatID = "", id
		}
	}
	after, err := rd.Start(ctx, q, *limit)
	if err != nil {
		return err
	}
	for {
		page, err := rd.List(ctx, q, after, auditPage)
		if err != nil {
			return err
		}
		for _, e := range page {
			if err := c.printEvent(e, *asJSON); err != nil {
				return err
			}
		}
		if len(page) < auditPage {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

type auditCmd struct{ configCmd }

// parseSince reads an RFC 3339 time, or a duration that counts back from now.
func parseSince(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is neither an RFC 3339 time nor a duration such as 24h", v)
}

// auditLine is the JSON line of one event (rule 21).
type auditLine struct {
	ID     int64           `json:"id"`
	At     string          `json:"at"`
	Kind   string          `json:"kind"`
	Flat   string          `json:"flat"`
	FlatID string          `json:"flat_id"`
	Name   string          `json:"name"`
	Via    string          `json:"via"`
	Actor  string          `json:"actor"`
	Data   json.RawMessage `json:"data,omitempty"`
}

func (c *auditCmd) printEvent(e store.AuditEvent, asJSON bool) error {
	at := e.Time.UTC().Format("2006-01-02T15:04:05.000Z")
	if asJSON {
		b, err := json.Marshal(auditLine{ID: e.ID, At: at, Kind: e.Kind, Flat: e.Flat, FlatID: e.FlatID, Name: e.Name, Via: e.Via, Actor: e.Actor, Data: e.Data})
		if err != nil {
			return err
		}
		c.printf("%s\n", b)
		return nil
	}
	target := e.Name
	switch {
	case e.Flat != "" && e.Name != "":
		target = e.Flat + "/" + e.Name
	case e.Flat != "":
		target = e.Flat
	case target == "":
		target = "-"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s  via=%s", at, e.Kind, target, e.Via)
	if e.Actor != "" {
		b.WriteString("  actor=" + strconv.Quote(e.Actor))
	}
	if e.FlatID != "" {
		b.WriteString("  flat_id=" + e.FlatID)
	}
	if len(e.Data) > 0 {
		b.WriteString("  data=" + string(e.Data))
	}
	c.printf("%s\n", b.String())
	return nil
}
