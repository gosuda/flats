package notebook

import "encoding/json"

type Operation interface {
	OperationName() string
	operationMeta() OperationMeta
}

type OperationMeta struct {
	UnitID    *string          `json:"unit_id,omitempty"`
	Structure []StructureCheck `json:"structure,omitempty"`
}

func (m OperationMeta) operationMeta() OperationMeta { return m }

type UndoOp struct{ OperationMeta }

func (UndoOp) OperationName() string { return "undo" }

type RedoOp struct{ OperationMeta }

func (RedoOp) OperationName() string { return "redo" }

type StructureCheck struct {
	Block  Target  `json:"block" kind:"block"`
	Page   Target  `json:"page" kind:"page"`
	Parent *Target `json:"parent" kind:"block" wire:"null"`
	After  *Target `json:"after,omitempty" kind:"block"`
}

// Placement distinguishes explicit append (Before == nil) from an after anchor.
// Its codec always emits before:null when neither anchor is set.
type Placement struct {
	Page   Target  `json:"page" kind:"page"`
	Parent *Target `json:"parent" kind:"block" wire:"null"`
	Before *Target `json:"before" kind:"block" wire:"null"`
	After  *Target `json:"after,omitempty" kind:"block"`
}

type CreateFields struct {
	Ref        string `json:"ref"`
	ProposedID *ID    `json:"proposed_id,omitempty"`
}

type CreatePageOp struct {
	OperationMeta
	CreateFields
	Parent *Target `json:"parent" kind:"page" wire:"null"`
	Before *Target `json:"before" kind:"page" wire:"null"`
	Title  string  `json:"title"`
}

func (CreatePageOp) OperationName() string { return "create_page" }

type SetPageTitleOp struct {
	OperationMeta
	Page  Target `json:"page" kind:"page"`
	Title string `json:"title"`
}

func (SetPageTitleOp) OperationName() string { return "set_page_title" }

type MovePageOp struct {
	OperationMeta
	Page   Target  `json:"page" kind:"page"`
	Parent *Target `json:"parent" kind:"page" wire:"null"`
	Before *Target `json:"before" kind:"page" wire:"null"`
}

func (MovePageOp) OperationName() string { return "move_page" }

type DeletePageOp struct {
	OperationMeta
	Page Target `json:"page" kind:"page"`
}

func (DeletePageOp) OperationName() string { return "delete_page" }

type DuplicatePageOp struct {
	OperationMeta
	Page Target `json:"page" kind:"page"`
}

func (DuplicatePageOp) OperationName() string { return "duplicate_page" }

type CreateBlockOp struct {
	OperationMeta
	CreateFields
	Place   Placement `json:"place"`
	Content Content   `json:"content"`
}

func (CreateBlockOp) OperationName() string { return "create_block" }

type SetBlockOp struct {
	OperationMeta
	Block   Target   `json:"block" kind:"block"`
	Content Content  `json:"content"`
	BaseV   *Version `json:"base_v,omitempty"`
}

func (SetBlockOp) OperationName() string { return "set_block" }

type TurnBlocksOp struct {
	OperationMeta
	Blocks []Target    `json:"blocks" kind:"block"`
	Kind   ContentKind `json:"kind"`
}

func (TurnBlocksOp) OperationName() string { return "turn_blocks" }

type MoveBlocksOp struct {
	OperationMeta
	Blocks []Target  `json:"blocks" kind:"block"`
	Place  Placement `json:"place"`
}

func (MoveBlocksOp) OperationName() string { return "move_blocks" }

type DeleteBlocksOp struct {
	OperationMeta
	Blocks []Target `json:"blocks" kind:"block"`
}

func (DeleteBlocksOp) OperationName() string { return "delete_blocks" }

type DuplicateBlocksOp struct {
	OperationMeta
	Blocks []Target `json:"blocks" kind:"block"`
	After  Target   `json:"after" kind:"block"`
}

func (DuplicateBlocksOp) OperationName() string { return "duplicate_blocks" }

type CreateDatabaseOp struct {
	OperationMeta
	CreateFields
	BlockRef                string    `json:"block_ref"`
	BlockProposedID         *ID       `json:"block_proposed_id,omitempty"`
	Place                   Placement `json:"place"`
	Title                   string    `json:"title"`
	TitlePropertyName       *string   `json:"title_property_name,omitempty"`
	TitlePropertyProposedID *ID       `json:"title_property_proposed_id,omitempty"`
	ViewProposedID          *ID       `json:"view_proposed_id,omitempty"`
}

func (CreateDatabaseOp) OperationName() string { return "create_database" }

type SetDatabaseTitleOp struct {
	OperationMeta
	Database Target `json:"database" kind:"database"`
	Title    string `json:"title"`
}

func (SetDatabaseTitleOp) OperationName() string { return "set_database_title" }

type CreatePropertyOp struct {
	OperationMeta
	CreateFields
	Database   Target      `json:"database" kind:"database"`
	Definition PropertyDef `json:"definition"`
}

func (CreatePropertyOp) OperationName() string { return "create_property" }

type SetPropertyOp struct {
	OperationMeta
	Property         Target      `json:"property" kind:"property"`
	Definition       PropertyDef `json:"definition"`
	ConfirmedPreview *string     `json:"confirmed_preview,omitempty"`
}

func (SetPropertyOp) OperationName() string { return "set_property" }

type DeletePropertyOp struct {
	OperationMeta
	Property Target `json:"property" kind:"property"`
}

func (DeletePropertyOp) OperationName() string { return "delete_property" }

type SetPropertyOrderOp struct {
	OperationMeta
	Database   Target   `json:"database" kind:"database"`
	Properties []Target `json:"properties" kind:"property"`
}

func (SetPropertyOrderOp) OperationName() string { return "set_property_order" }

type CreateOptionOp struct {
	OperationMeta
	CreateFields
	Property Target `json:"property" kind:"property"`
	Name     string `json:"name"`
	Color    Color  `json:"color"`
}

func (CreateOptionOp) OperationName() string { return "create_option" }

type SetOptionOp struct {
	OperationMeta
	Option Target `json:"option" kind:"option"`
	Name   string `json:"name"`
	Color  Color  `json:"color"`
}

func (SetOptionOp) OperationName() string { return "set_option" }

type DeleteOptionOp struct {
	OperationMeta
	Option Target `json:"option" kind:"option"`
}

func (DeleteOptionOp) OperationName() string { return "delete_option" }

type SetOptionOrderOp struct {
	OperationMeta
	Property Target   `json:"property" kind:"property"`
	Options  []Target `json:"options" kind:"option"`
}

func (SetOptionOrderOp) OperationName() string { return "set_option_order" }

// CellInputValue retains scalar ambiguity until the property's schema is known.
// Options are mutation targets, never the bare ids used by stored cells.
type CellInputValue struct {
	Null    bool
	Text    *string
	Bool    *bool
	Option  *Target  `kind:"option"`
	Options []Target `kind:"option"`
}

type CellInput struct {
	Property Target         `json:"property" kind:"property"`
	Value    CellInputValue `json:"value" wire:"null"`
}

type CreateRowOp struct {
	OperationMeta
	CreateFields
	Database Target      `json:"database" kind:"database"`
	Cells    []CellInput `json:"cells"`
	Before   *Target     `json:"before" kind:"row" wire:"null"`
	View     *Target     `json:"view,omitempty" kind:"view"`
}

func (CreateRowOp) OperationName() string { return "create_row" }

type SetCellOp struct {
	OperationMeta
	Row  Target    `json:"row" kind:"row"`
	Cell CellInput `json:"cell"`
}

func (SetCellOp) OperationName() string { return "set_cell" }

type MoveRowsOp struct {
	OperationMeta
	Rows   []Target `json:"rows" kind:"row"`
	Before *Target  `json:"before" kind:"row" wire:"null"`
}

func (MoveRowsOp) OperationName() string { return "move_rows" }

type DeleteRowsOp struct {
	OperationMeta
	Rows []Target `json:"rows" kind:"row"`
}

func (DeleteRowsOp) OperationName() string { return "delete_rows" }

type CreateViewOp struct {
	OperationMeta
	CreateFields
	Database   Target  `json:"database" kind:"database"`
	Definition ViewDef `json:"definition"`
}

func (CreateViewOp) OperationName() string { return "create_view" }

type SetViewOp struct {
	OperationMeta
	View       Target  `json:"view" kind:"view"`
	Definition ViewDef `json:"definition"`
}

func (SetViewOp) OperationName() string { return "set_view" }

type DeleteViewOp struct {
	OperationMeta
	View Target `json:"view" kind:"view"`
}

func (DeleteViewOp) OperationName() string { return "delete_view" }

type SetViewOrderOp struct {
	OperationMeta
	Database Target   `json:"database" kind:"database"`
	Views    []Target `json:"views" kind:"view"`
}

func (SetViewOrderOp) OperationName() string { return "set_view_order" }

type AssetInput struct {
	Path  string      `json:"path"`
	Asset AssetTarget `json:"asset"`
}
type AssetTarget struct {
	Hash string
	Tmp  string
}
type ProposedItem struct {
	Ref        string `json:"ref"`
	ProposedID ID     `json:"proposed_id"`
}
type MarkdownToBlocksOp struct {
	OperationMeta
	Place       Placement      `json:"place"`
	Markdown    string         `json:"markdown"`
	AssetRefs   []AssetInput   `json:"asset_refs,omitempty"`
	ProposedIDs []ProposedItem `json:"proposed_ids,omitempty"`
}

func (MarkdownToBlocksOp) OperationName() string { return "markdown_to_blocks" }

type State struct {
	Store string   `json:"store"`
	Seq   Sequence `json:"seq"`
}
type ItemGuard struct {
	Ref Ref     `json:"ref"`
	Ver Version `json:"ver"`
}
type Guards struct {
	State *State      `json:"state,omitempty"`
	Items []ItemGuard `json:"items,omitempty"`
}
type UploadAsset struct {
	Ref  string `json:"ref"`
	Data []byte `json:"data"`
}

type Update struct {
	Slug    string        `json:"slug"`
	OpID    string        `json:"op_id"`
	Message *string       `json:"message,omitempty"`
	Guards  *Guards       `json:"guards,omitempty"`
	Ops     []Operation   `json:"ops"`
	Assets  []UploadAsset `json:"assets,omitempty"`
}

type Action struct {
	N           Sequence    `json:"n"`
	ActionID    string      `json:"action_id"`
	TypingBlock *Ref        `json:"typing_block,omitempty" kind:"block"`
	Close       *bool       `json:"close,omitempty"`
	Ops         []Operation `json:"ops"`
}

func (u Update) MarshalJSON() ([]byte, error) { return EncodeUpdate(u) }
func (u *Update) UnmarshalJSON(data []byte) error {
	v, err := DecodeUpdate(data)
	if err == nil {
		*u = v
	}
	return err
}
func (a Action) MarshalJSON() ([]byte, error) { return EncodeAction(a) }
func (a *Action) UnmarshalJSON(data []byte) error {
	v, err := DecodeAction(data)
	if err == nil {
		*a = v
	}
	return err
}

type rawOperations []json.RawMessage
