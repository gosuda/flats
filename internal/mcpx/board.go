package mcpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/core"
)

func registerBoard(s *mcp.Server, t *tools) {
	no := false
	write := &mcp.ToolAnnotations{DestructiveHint: &no}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "get_board", Annotations: ro,
		Description: "Read a board (an infinite-canvas whiteboard people and agents edit live; guide topic.board) as structured data: elements with their ver, seq (the current state), revision and pending. " +
			"Narrow it by frame, rect [x, y, w, h] or ids; format outline lists types, text and connections cheaply. Pages hold limit elements (default 500); pass next as cursor. revision reads an earlier revision. Read before you change a board."}, t.getBoard)
	mcp.AddTool(s, &mcp.Tool{Name: "update_board", Annotations: write,
		Description: "Write a board, creating it when the slug is missing: add, set, reparent, delete, group, ungroup, zorder and delete_frame_contents operations, layout help and images (assets). " +
			"All operations apply or none, as one revision, at once: no Draft, publish or approval; on a Public board everyone sees it immediately. Guard with if_seq or if_ver; op_id (8-120 characters) makes a resend safe. Guide topic.board."}, t.updateBoard)
	mcp.AddTool(s, &mcp.Tool{Name: "get_board_share", Annotations: ro,
		Description: "Read a selection a person sent from the board with Send to agent (\"#7\" is n 7): its elements now, with deleted ones listed as missing. Without n, the recent selections. Shares last 24 hours or until the board restarts (guide topic.board)."}, t.getBoardShare)
	mcp.AddTool(s, &mcp.Tool{Name: "import_board", Annotations: write,
		Description: "Import a Flats board data file (.zip as base64, at most the host upload limit) into a board, creating it when the slug is missing: mode add copies the elements with new ids to the right of the board, mode replace replaces the whole board. One revision, applied at once (guide topic.board)."}, t.importBoard)
}

// GetBoardIn reads a board.
type GetBoardIn struct {
	Slug     string    `json:"slug" jsonschema:"board slug"`
	Revision int       `json:"revision,omitempty" jsonschema:"read this earlier revision instead of the current board"`
	Frame    string    `json:"frame,omitempty" jsonschema:"only this frame and its members"`
	Rect     []float64 `json:"rect,omitempty" jsonschema:"only elements that intersect this area: [x, y, w, h] in canvas px"`
	IDs      []string  `json:"ids,omitempty" jsonschema:"only these element ids"`
	Format   string    `json:"format,omitempty" jsonschema:"full (default: coordinates, styles and ver) or outline (types, text and connections)"`
	Cursor   string    `json:"cursor,omitempty" jsonschema:"the previous page's next, to read the following page"`
	Limit    int       `json:"limit,omitempty" jsonschema:"elements per page (default 500, at most 2000)"`
}

// GetBoardOut is one page of a board read.
type GetBoardOut struct {
	Seq      int64            `json:"seq,omitempty" jsonschema:"identifier of the current state, for if_seq"`
	Revision int              `json:"revision" jsonschema:"the revision read, or the latest revision for the current board"`
	Pending  bool             `json:"pending" jsonschema:"true when people's edits are not a revision yet"`
	Meta     map[string]any   `json:"meta,omitempty" jsonschema:"board metadata: title and grid"`
	Elements []map[string]any `json:"elements"`
	Next     string           `json:"next,omitempty" jsonschema:"pass as cursor for the next page"`
}

func (t *tools) getBoard(ctx context.Context, _ *mcp.CallToolRequest, in GetBoardIn) (*mcp.CallToolResult, GetBoardOut, error) {
	q, err := core.BoardQuery(in.Frame, in.Rect, in.IDs, in.Format, in.Cursor, in.Limit)
	if err != nil {
		return nil, GetBoardOut{}, toolErr(err, "fix the filter (guide topic.board)")
	}
	p, err := t.svc.GetBoard(ctx, in.Slug, in.Revision, q)
	if err != nil {
		return nil, GetBoardOut{}, toolErr(err, boardHint(err, in.Slug))
	}
	out := GetBoardOut{Seq: p.Seq, Revision: p.Revision, Pending: p.Pending, Next: p.Next, Elements: make([]map[string]any, 0, len(p.Elements))}
	if len(p.Meta) > 0 {
		_ = json.Unmarshal(p.Meta, &out.Meta)
	}
	for _, raw := range p.Elements {
		var el map[string]any
		if err := json.Unmarshal(raw, &el); err == nil {
			out.Elements = append(out.Elements, el)
		}
	}
	var b strings.Builder
	if in.Revision > 0 {
		fmt.Fprintf(&b, "Board %s as of revision %d: %d elements on this page.", in.Slug, in.Revision, len(out.Elements))
	} else {
		fmt.Fprintf(&b, "Board %s at seq %d (revision %d): %d elements on this page.", in.Slug, out.Seq, out.Revision, len(out.Elements))
		if out.Pending {
			b.WriteString(" People have edits that are not a revision yet.")
		}
	}
	if out.Next != "" {
		b.WriteString(" More: pass next as cursor.")
	}
	return result(b.String(), out), out, nil
}

// UpdateBoardIn is one update_board call.
type UpdateBoardIn struct {
	Slug    string           `json:"slug" jsonschema:"board slug (created when missing)"`
	OpID    string           `json:"op_id" jsonschema:"8 to 120 characters naming this call; resend the same request with the same op_id after a timeout"`
	Ops     []map[string]any `json:"ops" jsonschema:"operations applied in order: add, set, reparent, delete, group, ungroup, zorder, delete_frame_contents (guide topic.board)"`
	Layout  []map[string]any `json:"layout,omitempty" jsonschema:"layout help after the operations: {kind: grid|row|column, ids, in?, columns?, gap?}"`
	Assets  []BoardAssetIn   `json:"assets,omitempty" jsonschema:"images for image elements of this call; an element's asset names an entry's ref"`
	IfSeq   *int64           `json:"if_seq,omitempty" jsonschema:"seq you read; refused when anything on the board changed since"`
	Message string           `json:"message,omitempty" jsonschema:"short note shown in the board's history"`
	Replace bool             `json:"replace,omitempty" jsonschema:"replace the whole board with this call's add operations; only when the user asked for a fresh board"`
}

// BoardAssetIn is one image of an update_board call.
type BoardAssetIn struct {
	Ref     string `json:"ref,omitempty" jsonschema:"a name starting with $ that image elements of this call give as asset, e.g. $logo"`
	Content string `json:"content" jsonschema:"the image as base64: PNG, JPEG, GIF or still WebP, at most the upload limit"`
}

// UpdateBoardOut reports an applied board write.
type UpdateBoardOut struct {
	Revision *int                 `json:"revision" jsonschema:"the revision this call created (the latest when nothing changed); null while it is being recorded"`
	Seq      int64                `json:"seq" jsonschema:"the board's state after the call, for if_seq"`
	IDs      any                  `json:"ids" jsonschema:"update_board: the new id of each ref; import_board: the new element ids"`
	Layout   map[string][]float64 `json:"layout,omitempty" jsonschema:"the coordinates layout help gave, {id: [x, y]}"`
	Results  any                  `json:"results,omitempty" jsonschema:"the result of each operation"`
	Assets   map[string]string    `json:"assets,omitempty" jsonschema:"the stored hash of each asset ref"`
	Created  bool                 `json:"created,omitempty"`
	Warning  string               `json:"warning,omitempty"`
	Flat     *FlatInfo            `json:"flat,omitempty"`
}

// boardUpdateFromJSON decodes update_board operations and layouts given as
// JSON objects.
func boardUpdateFromJSON(ops, layout any) ([]engine.AgentOp, []engine.Layout, error) {
	var outOps []engine.AgentOp
	var outLayout []engine.Layout
	for _, x := range []struct {
		in  any
		dst any
	}{{ops, &outOps}, {layout, &outLayout}} {
		raw, err := json.Marshal(x.in)
		if err != nil {
			return nil, nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(x.dst); err != nil {
			return nil, nil, refuse(core.ErrInvalid, "malformed operation or layout: %v", err)
		}
	}
	return outOps, outLayout, nil
}

func (t *tools) updateBoard(ctx context.Context, _ *mcp.CallToolRequest, in UpdateBoardIn) (*mcp.CallToolResult, UpdateBoardOut, error) {
	ops, layout, err := boardUpdateFromJSON(in.Ops, in.Layout)
	if err != nil {
		return nil, UpdateBoardOut{}, toolErr(err, "nothing changed; fix the operations (guide topic.board)")
	}
	u := core.BoardUpdate{OpID: in.OpID, Ops: ops, Layout: layout, IfSeq: in.IfSeq, Message: in.Message, Replace: in.Replace}
	for i, a := range in.Assets {
		data, err := decodeBase64(a.Content)
		if err != nil {
			return nil, UpdateBoardOut{}, toolErr(refuse(core.ErrInvalid, "assets[%d] is not base64: %v", i, err), "nothing changed; send each image as standard base64")
		}
		u.Assets = append(u.Assets, core.BoardAsset{Ref: a.Ref, Content: data})
	}
	res, err := t.svc.UpdateBoard(ctx, in.Slug, u, core.ViaMCP)
	if err != nil {
		return nil, UpdateBoardOut{}, toolErr(err, boardHint(err, in.Slug))
	}
	out := t.boardWriteOut(ctx, in.Slug, res)
	var b strings.Builder
	switch {
	case res.Created:
		fmt.Fprintf(&b, "Created board %s and applied the call.", in.Slug)
	case res.Replayed:
		fmt.Fprintf(&b, "op_id %s was already applied; this is its first result and nothing changed again.", in.OpID)
	default:
		fmt.Fprintf(&b, "Applied the call to board %s.", in.Slug)
	}
	writeBoardResult(&b, out)
	return result(b.String(), out), out, nil
}

// boardWriteOut shapes a board write's result for MCP.
func (t *tools) boardWriteOut(ctx context.Context, slug string, res core.BoardWriteResult) UpdateBoardOut {
	out := UpdateBoardOut{Revision: res.Revision, Seq: res.Seq, IDs: res.IDs, Assets: res.Assets, Created: res.Created, Warning: res.Warning}
	if len(res.Layout) > 0 {
		out.Layout = map[string][]float64{}
		for id, p := range res.Layout {
			out.Layout[id] = []float64{p[0], p[1]}
		}
	}
	if len(res.Results) > 0 {
		_ = json.Unmarshal(res.Results, &out.Results)
	}
	if f, err := t.svc.GetFlat(ctx, slug); err == nil {
		fi := flatInfo(f)
		out.Flat = &fi
	}
	return out
}

func writeBoardResult(b *strings.Builder, out UpdateBoardOut) {
	if out.Revision != nil {
		fmt.Fprintf(b, " Revision %d, seq %d.", *out.Revision, out.Seq)
	} else {
		fmt.Fprintf(b, " Seq %d.", out.Seq)
	}
	if out.Warning != "" {
		b.WriteString(" Warning: " + out.Warning + ".")
	}
	if f := out.Flat; f != nil {
		fmt.Fprintf(b, " It is live, %s, at %s; there is no publish step.", f.Visibility, f.PrivateURL)
		if f.Visibility == "public" {
			b.WriteString(" This board is Public: everyone on the internet sees the change now.")
		}
		writePending(b, f.PrivateState, f.PrivateDetail)
	}
}

// GetBoardShareIn reads shared selections.
type GetBoardShareIn struct {
	Slug string `json:"slug" jsonschema:"board slug"`
	N    int64  `json:"n,omitempty" jsonschema:"the share number the person gave (#7 is 7); omit for the recent shares"`
}

// BoardShareOut is one shared selection.
type BoardShareOut struct {
	N        int64            `json:"n"`
	IDs      []string         `json:"ids"`
	Label    string           `json:"label" jsonschema:"who sent it"`
	Verified bool             `json:"verified" jsonschema:"true when the sender's identity comes from Tailscale"`
	At       time.Time        `json:"at"`
	Elements []map[string]any `json:"elements" jsonschema:"the shared elements as they are now"`
	Missing  []string         `json:"missing" jsonschema:"shared ids deleted since, or removed by a rollback"`
}

// GetBoardShareOut lists shared selections.
type GetBoardShareOut struct {
	Shares []BoardShareOut `json:"shares"`
}

func (t *tools) getBoardShare(ctx context.Context, _ *mcp.CallToolRequest, in GetBoardShareIn) (*mcp.CallToolResult, GetBoardShareOut, error) {
	views, err := t.svc.BoardShares(ctx, in.Slug, in.N)
	if err != nil {
		return nil, GetBoardShareOut{}, toolErr(err, boardHint(err, in.Slug))
	}
	out := GetBoardShareOut{Shares: make([]BoardShareOut, 0, len(views))}
	for _, v := range views {
		s := BoardShareOut{N: v.N, IDs: v.IDs, Label: v.Label, Verified: v.Verified, At: time.Unix(v.At, 0).UTC(), Missing: v.Missing, Elements: []map[string]any{}}
		for _, raw := range v.Elements {
			var el map[string]any
			if json.Unmarshal(raw, &el) == nil {
				s.Elements = append(s.Elements, el)
			}
		}
		out.Shares = append(out.Shares, s)
	}
	var text string
	switch {
	case in.N > 0 && len(out.Shares) == 1:
		s := out.Shares[0]
		text = fmt.Sprintf("Share #%d from %s: %d elements, %d missing.", s.N, s.Label, len(s.Elements), len(s.Missing))
	default:
		text = fmt.Sprintf("%d recent shares of board %s.", len(out.Shares), in.Slug)
	}
	return result(text, out), out, nil
}

// ImportBoardIn imports a board data file.
type ImportBoardIn struct {
	Slug    string `json:"slug" jsonschema:"board slug (created when missing)"`
	OpID    string `json:"op_id" jsonschema:"8 to 120 characters naming this import; resend with the same op_id after a timeout"`
	Archive string `json:"archive" jsonschema:"the Flats board data file (.zip) as base64, at most the host upload limit"`
	Mode    string `json:"mode" jsonschema:"add (copy the elements with new ids next to the board) or replace (the whole board)"`
}

func (t *tools) importBoard(ctx context.Context, _ *mcp.CallToolRequest, in ImportBoardIn) (*mcp.CallToolResult, UpdateBoardOut, error) {
	data, err := decodeBase64(in.Archive)
	if err != nil {
		return nil, UpdateBoardOut{}, toolErr(refuse(core.ErrInvalid, "archive is not base64: %v", err), "send the .zip as standard base64")
	}
	if limit := t.svc.UploadLimit(); limit > 0 && int64(len(data)) > limit {
		return nil, UpdateBoardOut{}, toolErr(refuse(core.ErrInvalid, "the archive is %d bytes, over the %d-byte upload limit", len(data), limit), "import larger files with the CLI: flats board import")
	}
	res, err := t.svc.ImportBoard(ctx, in.Slug, core.BoardImport{OpID: in.OpID, Mode: in.Mode, Archive: bytes.NewReader(data), Size: int64(len(data))}, core.ViaMCP)
	if err != nil {
		return nil, UpdateBoardOut{}, toolErr(err, boardHint(err, in.Slug))
	}
	out := t.boardWriteOut(ctx, in.Slug, res)
	var b strings.Builder
	if res.Replayed {
		fmt.Fprintf(&b, "op_id %s was already imported; this is its first result.", in.OpID)
	} else {
		fmt.Fprintf(&b, "Imported the data file into board %s (%s).", in.Slug, in.Mode)
	}
	writeBoardResult(&b, out)
	return result(b.String(), out), out, nil
}

// boardHint tells the caller what to do after a refused board call.
func boardHint(err error, slug string) string {
	switch core.ErrorCategory(err) {
	case "not_board":
		return "board tools need a board; check get_flat, or create one with create_flat {type: board}"
	case "conflict":
		return "nothing changed; read get_board again (from the first page for an expired cursor) and rebuild the call against what people wrote"
	case "board_capacity":
		return "nothing changed; the board is at its disk quota, which also holds the change's revision. Tell the user: the operator can raise the per-flat disk quota, or free space by lowering the revision quota or unpinning revisions"
	case "invalid":
		return "nothing changed; fix the call (guide topic.board)"
	case "unavailable":
		return "wait a few seconds and retry with the same op_id"
	case "not_found":
		return notFoundHint(err, slug)
	}
	return ""
}
