package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"modernc.org/sqlite"
)

type fixtureState struct {
	Journal     string   `json:"journal_mode"`
	ForeignKeys int      `json:"foreign_keys"`
	Synchronous int      `json:"synchronous"`
	Versions    []string `json:"versions"`
}

func seed() (fixtureState, error) {
	var state fixtureState
	if len(os.Args) != 3 {
		return state, fmt.Errorf("expected database and property")
	}
	if err := sqlite.RegisterCollationUtf8("notebook_u64", func(a, b string) int {
		if len(a) < len(b) {
			return -1
		}
		if len(a) > len(b) {
			return 1
		}
		return strings.Compare(a, b)
	}); err != nil {
		return state, err
	}
	u := url.URL{Scheme: "file", Path: os.Args[1]}
	db, err := sql.Open("sqlite", u.String()+"?mode=rw&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate")
	if err != nil {
		return state, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	tx, err := db.Begin()
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	for {
		var batch json.RawMessage
		err := decoder.Decode(&batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			return state, err
		}
		encoded := string(batch)
		if _, err := tx.Exec(`INSERT INTO allocated_ids(id,kind)
			SELECT json_extract(value,'$.id'),'option' FROM json_each(?)`, encoded); err != nil {
			return state, err
		}
		if _, err := tx.Exec(`INSERT INTO items(id,kind,inc,alive,deleted_seq,own_version,aggregate_version,data)
			SELECT json_extract(value,'$.id'),'option','7',1,'0','7','7',
			json_object('property',?,'name',json_extract(value,'$.name'),'color','gray') FROM json_each(?)`, os.Args[2], encoded); err != nil {
			return state, err
		}
		if _, err := tx.Exec(`INSERT INTO options(id,property_id,name_key,alive)
			SELECT json_extract(value,'$.id'),?,json_extract(value,'$.name'),1 FROM json_each(?)`, os.Args[2], encoded); err != nil {
			return state, err
		}
	}
	if err := tx.Commit(); err != nil {
		return state, err
	}
	for _, query := range []struct {
		sql string
		out any
	}{{`PRAGMA journal_mode`, &state.Journal}, {`PRAGMA foreign_keys`, &state.ForeignKeys}, {`PRAGMA synchronous`, &state.Synchronous}} {
		if err := db.QueryRow(query.sql).Scan(query.out); err != nil {
			return state, err
		}
	}
	rows, err := db.Query(`SELECT CAST(value AS TEXT) FROM json_each('["18446744073709551615","100","10","1","99","9","2"]') ORDER BY CAST(value AS TEXT) COLLATE notebook_u64`)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return state, err
		}
		state.Versions = append(state.Versions, version)
	}
	return state, rows.Err()
}

func main() {
	state, err := seed()
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(state)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
