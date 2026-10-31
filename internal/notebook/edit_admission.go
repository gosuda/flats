package notebook

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type editLimits struct {
	Pages                 int64
	BlocksPerPage         int64
	PageBytes             int64
	Text                  int64
	Code                  int64
	Alt                   int64
	Title                 int64
	Databases             int64
	RowsPerDatabase       int64
	PropertiesPerDatabase int64
	OptionsPerProperty    int64
	ViewsPerDatabase      int64
	DatabaseBytes         int64
	Cell                  int64
	Name                  int64
	URL                   int64
}

func defaultEditLimits() editLimits {
	return editLimits{
		Pages: MaxPages, BlocksPerPage: MaxBlocksPerPage, PageBytes: MaxPageBytes,
		Text: MaxRichTextCodePoints, Code: MaxCodeCodePoints, Alt: MaxAltCodePoints, Title: MaxTitleCodePoints,
		Databases: MaxDatabases, RowsPerDatabase: MaxRowsPerDatabase,
		PropertiesPerDatabase: MaxPropertiesPerDatabase, OptionsPerProperty: MaxOptionsPerProperty,
		ViewsPerDatabase: MaxViewsPerDatabase, DatabaseBytes: MaxDatabaseBytes,
		Cell: MaxCellCodePoints, Name: MaxNameCodePoints, URL: MaxURLBytes,
	}
}

type usageKey struct {
	Owner  ID
	Metric string
}
type editUsage map[usageKey]int64

func sequenceString(seq Sequence) string { return strconv.FormatUint(uint64(seq), 10) }

func measureEditUsage(ctx context.Context, tx *sql.Tx) (editUsage, error) {
	usage := editUsage{}
	rows, err := tx.QueryContext(ctx, `SELECT id,data FROM items WHERE alive=1 AND kind IN ('page','block') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type bucketKey struct {
		Page   ID
		Bucket byte
	}
	buckets := map[bucketKey]int64{}
	for rows.Next() {
		var id ID
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if id.Kind() == PageKind {
			var page Page
			if err := strictDecode([]byte(raw), &page); err != nil {
				return nil, err
			}
			usage[usageKey{Metric: "pages"}]++
			usage[usageKey{Owner: id, Metric: "title"}] = int64(utf8.RuneCountInString(page.Title))
			continue
		}
		var block Block
		if err := strictDecode([]byte(raw), &block); err != nil {
			return nil, err
		}
		usage[usageKey{Owner: block.Page, Metric: "blocks"}]++
		text := block.Content.Text
		if block.Content.Kind == Image {
			text = block.Content.Caption
		}
		var characters int64
		for _, run := range text {
			characters += int64(utf8.RuneCountInString(run.Text))
		}
		usage[usageKey{Owner: id, Metric: "text"}] = characters
		usage[usageKey{Owner: id, Metric: "code"}] = int64(utf8.RuneCountInString(block.Content.Code))
		usage[usageKey{Owner: id, Metric: "alt"}] = int64(utf8.RuneCountInString(block.Content.Alt))
		usage[usageKey{Owner: id, Metric: "language"}] = int64(utf8.RuneCountInString(block.Content.Language))
		for _, run := range text {
			if run.Link != nil {
				key := usageKey{Owner: id, Metric: "url"}
				usage[key] = max(usage[key], int64(len(run.Link.URL)))
				usage[usageKey{Owner: id, Metric: "url_excess"}] += max(int64(len(run.Link.URL))-MaxURLBytes, 0)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil, err
		}
		fields["id"], err = json.Marshal(id)
		if err != nil {
			return nil, err
		}
		encoded, err := CanonicalJSON(fields)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(id))
		bucket := bucketKey{Page: block.Page, Bucket: hash[0]}
		if buckets[bucket] == 0 {
			usage[usageKey{Owner: block.Page, Metric: "page_bytes"}] += 2
		} else {
			usage[usageKey{Owner: block.Page, Metric: "page_bytes"}]++
		}
		buckets[bucket]++
		usage[usageKey{Owner: block.Page, Metric: "page_bytes"}] += int64(len(encoded))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := measureDatabaseUsage(ctx, tx, usage); err != nil {
		return nil, err
	}
	return usage, nil
}

func measureDatabaseUsage(ctx context.Context, tx *sql.Tx, usage editUsage) error {
	pages := map[ID]ID{}
	rows, err := tx.QueryContext(ctx, `SELECT d.id,b.page FROM databases d JOIN items i ON i.id=d.id JOIN blocks b ON b.id=d.block WHERE i.alive=1 ORDER BY d.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, page ID
		if err := rows.Scan(&id, &page); err != nil {
			return err
		}
		pages[id] = page
		usage[usageKey{Metric: "databases"}]++
		data, err := encodeContentSchema(ctx, tx, id)
		if err != nil {
			return err
		}
		usage[usageKey{Owner: page, Metric: "page_bytes"}] += int64(len(data))
		record, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		usage[usageKey{Owner: id, Metric: "title"}] = int64(utf8.RuneCountInString(record.Item.Data.(Database).Title))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id FROM items WHERE alive=1 AND kind IN ('property','option','view','row') ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type rowBucket struct {
		Database ID
		Bucket   string
	}
	buckets := map[rowBucket]bool{}
	for rows.Next() {
		var id ID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		record, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		switch data := record.Item.Data.(type) {
		case Property:
			usage[usageKey{Owner: data.Database, Metric: "properties"}]++
			usage[usageKey{Owner: id, Metric: "name"}] = int64(utf8.RuneCountInString(data.Definition.Name))
		case Option:
			usage[usageKey{Owner: data.Property, Metric: "options"}]++
			usage[usageKey{Owner: id, Metric: "name"}] = int64(utf8.RuneCountInString(data.Name))
		case View:
			usage[usageKey{Owner: data.Database, Metric: "views"}]++
			usage[usageKey{Owner: id, Metric: "name"}] = int64(utf8.RuneCountInString(data.Definition.Name))
		case Row:
			usage[usageKey{Owner: data.Database, Metric: "rows"}]++
			for property, cell := range data.Cells {
				var length int
				switch value := cell.Value.(type) {
				case string:
					length = utf8.RuneCountInString(value)
				case Decimal:
					length = len(value)
				}
				usage[usageKey{Owner: id, Metric: "cell:" + string(property)}] = int64(length)
				if cell.Type == URLType && cell.Value != nil {
					usage[usageKey{Owner: id, Metric: "cell_url:" + string(property)}] = int64(len(cell.Value.(string)))
				}
			}
			fields, err := contentItem(record.Item)
			if err != nil {
				return err
			}
			encoded, err := CanonicalJSON(fields)
			if err != nil {
				return err
			}
			bucket := rowBucket{Database: data.Database, Bucket: contentBucket(id)}
			size := int64(len(encoded))
			if buckets[bucket] {
				size++
			} else {
				size += 2
				buckets[bucket] = true
			}
			usage[usageKey{Owner: data.Database, Metric: "database_bytes"}] += size
			usage[usageKey{Owner: pages[data.Database], Metric: "page_bytes"}] += size
		}
	}
	return rows.Err()
}

func checkEditUsage(ctx context.Context, tx *sql.Tx, before editUsage, configured ...*editLimits) error {
	after, err := measureEditUsage(ctx, tx)
	if err != nil {
		return err
	}
	limits := defaultEditLimits()
	if len(configured) > 0 && configured[0] != nil {
		limits = *configured[0]
	}
	caps := map[string]int64{
		"pages": limits.Pages, "blocks": limits.BlocksPerPage, "page_bytes": limits.PageBytes,
		"text": limits.Text, "code": limits.Code, "alt": limits.Alt, "title": limits.Title,
		"language": limits.Name, "url": limits.URL, "url_excess": 0,
		"databases": limits.Databases, "rows": limits.RowsPerDatabase,
		"properties": limits.PropertiesPerDatabase, "options": limits.OptionsPerProperty,
		"views": limits.ViewsPerDatabase, "database_bytes": limits.DatabaseBytes,
		"name": limits.Name, "cell": limits.Cell, "cell_url": limits.URL,
	}
	for key, value := range after {
		metric, _, _ := strings.Cut(key.Metric, ":")
		if err := CheckAdmission(before[key], value, caps[metric]); err != nil {
			return &editFailure{Status: "refused", Reason: "item_limit", Detail: fmt.Sprintf("%s %s: %v", key.Owner, key.Metric, err)}
		}
	}
	return nil
}

func beginUndoAdmission(ctx context.Context, tx *sql.Tx) (editUsage, error) {
	usage, err := measureEditUsage(ctx, tx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_undo_action`); err != nil {
		return nil, err
	}
	return usage, nil
}

func finishUndoAdmission(ctx context.Context, tx *sql.Tx, before editUsage, summary undoSummary, opts editOptions, err error) (undoSummary, error) {
	if err == nil {
		err = checkEditUsage(ctx, tx, before, opts.Limits)
	}
	if err != nil {
		_, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO notebook_undo_action`)
		_, releaseErr := tx.ExecContext(ctx, `RELEASE notebook_undo_action`)
		return undoSummary{}, errors.Join(err, rollbackErr, releaseErr)
	}
	_, err = tx.ExecContext(ctx, `RELEASE notebook_undo_action`)
	return summary, err
}
