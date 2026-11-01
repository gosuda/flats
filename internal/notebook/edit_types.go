package notebook

import (
	"context"
	"database/sql"
	"time"
)

type operationResult struct {
	Index       int     `json:"index"`
	UnitID      *string `json:"unit_id,omitempty"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	FailedIndex *int    `json:"failed_index,omitempty"`
}

type copyMapping struct {
	Index  int     `json:"index"`
	Source Ref     `json:"source"`
	Ref    Ref     `json:"ref"`
	Ver    Version `json:"ver"`
}

type overwrite struct {
	Block   Ref
	Version Version
	Base    Version
}

type editResult struct {
	Results         []operationResult
	Refs            map[string]Ref
	Copies          []copyMapping
	CopyRoots       []copyMapping
	Warnings        []Warning
	MarkdownCreates []markdownCreation
	Overwrote       []overwrite
	Records         map[ID]itemRecord
}

type editOptions struct {
	Browser         bool
	Session         string
	Seq             Sequence
	ActionID        string
	TypingBlock     *Ref
	Close           bool
	Now             time.Time
	Guards          *Guards
	UndoBudget      int64
	Limits          *editLimits
	admissionBefore editUsage
}

type itemChange struct {
	Before    *itemRecord
	After     itemRecord
	Groups    []GroupKey
	Component ID
}

type edit struct {
	ctx            context.Context
	tx             *sql.Tx
	seq            Sequence
	session        string
	browser        bool
	refs           map[string]Ref
	result         editResult
	changes        map[ID]itemChange
	dbLineage      map[ID]undoAggregate
	pageBefore     map[ID]Version
	index          int
	oldPlaces      map[ID]Item
	effects        map[ID]itemRecord
	component      ID
	markdown       markdownEditState
	deferAdmission bool
}

type editFailure struct {
	Status string
	Reason string
	Detail string
}

func (f *editFailure) Error() string { return f.Status + ":" + f.Reason }
func gone() error                    { return &editFailure{Status: "ignored", Reason: "gone"} }
func invalidStructure() error        { return &editFailure{Status: "refused", Reason: "structure"} }
func invalidKind() error             { return &editFailure{Status: "ignored", Reason: "kind"} }
