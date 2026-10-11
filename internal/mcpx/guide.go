package mcpx

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
)

// internalCategory names tool errors that have no specific core category.
const internalCategory = "internal"

// GuideIn selects guide items.
type GuideIn struct {
	Items []string `json:"items,omitempty" jsonschema:"items such as topic.index, topic.server.db or refusal.conflict; several per call; empty reads topic.index"`
}

// GuideItem is one guide page.
type GuideItem struct {
	ID       string `json:"id"`
	Markdown string `json:"markdown"`
}

// GuideOut returns the requested guide pages.
type GuideOut struct {
	Items     []GuideItem `json:"items"`
	Unknown   []string    `json:"unknown,omitempty" jsonschema:"requested items that do not exist"`
	Available []string    `json:"available,omitempty" jsonschema:"every valid item; returned with topic.index or when an item is unknown"`
}

func registerGuide(s *mcp.Server, svc *core.Service) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "guide",
		Description: "Read Flats guidance on demand. topic.index routes by task; other items: topic.static, topic.server, " +
			"topic.server.types, topic.server.files, topic.server.db, topic.server.fetch, topic.server.limits, topic.docs, topic.manifest, " +
			"topic.env-secrets, topic.approvals, topic.rollback-data, topic.preview-verify, topic.preview-check, topic.instructions, and " +
			"refusal.<category> for a refused call. Pass several items at once. No side effects.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in GuideIn) (*mcp.CallToolResult, GuideOut, error) {
		out := guide(in.Items, svc.UploadLimit())
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: guideText(out)}}}, out, nil
	})
}

// guideItems lists every valid item: topics in guide order, the generated
// instructions, then one refusal page per error category.
func guideItems() []string {
	items := make([]string, 0, 40)
	for _, t := range runtimeref.TopicOrder {
		items = append(items, "topic."+t)
	}
	items = append(items, "topic.instructions")
	for _, c := range runtimeref.RefusalCategories() {
		items = append(items, "refusal."+c)
	}
	return items
}

// guide resolves items. A bare topic name ("server.db") means its topic;
// duplicates are read once.
func guide(items []string, uploadLimit int64) GuideOut {
	if len(items) == 0 {
		items = []string{"topic.index"}
	}
	out := GuideOut{Items: []GuideItem{}}
	seen := map[string]bool{}
	refusalSeen := false
	for _, raw := range items {
		id := strings.ToLower(strings.TrimSpace(raw))
		if !strings.HasPrefix(id, "topic.") && !strings.HasPrefix(id, "refusal.") {
			id = "topic." + id
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		md, ok := guidePage(id, uploadLimit)
		if !ok {
			out.Unknown = append(out.Unknown, raw)
			continue
		}
		if strings.HasPrefix(id, "refusal.") && !refusalSeen {
			md, refusalSeen = runtimeref.RefusalIntro+"\n"+md, true
		}
		out.Items = append(out.Items, GuideItem{ID: id, Markdown: md})
		if id == "topic.index" {
			out.Available = guideItems()
		}
	}
	if len(out.Unknown) > 0 {
		out.Available = guideItems()
	}
	return out
}

func guidePage(id string, uploadLimit int64) (string, bool) {
	if name, ok := strings.CutPrefix(id, "refusal."); ok {
		return runtimeref.Refusal(name)
	}
	name := strings.TrimPrefix(id, "topic.")
	if name == "instructions" {
		return "## Server instructions\n\nThe MCP server sends this text at initialization.\n\n" + instructions(uploadLimit) + "\n", true
	}
	return runtimeref.Topic(name)
}

func guideText(out GuideOut) string {
	var b strings.Builder
	for i, it := range out.Items {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "# %s\n\n%s", it.ID, it.Markdown)
	}
	if len(out.Unknown) > 0 {
		fmt.Fprintf(&b, "\nUnknown guide items: %s.\n", strings.Join(out.Unknown, ", "))
	}
	if len(out.Available) > 0 {
		fmt.Fprintf(&b, "\nAvailable guide items: %s\n", strings.Join(out.Available, ", "))
	}
	return b.String()
}

// errorCategory classifies every tool error: the core category, invalid for
// upload validation, and internal for anything else.
func errorCategory(err error) string {
	if c := core.ErrorCategory(err); c != "" {
		return c
	}
	if _, ok := bundle.IsValidation(err); ok {
		return "invalid"
	}
	return internalCategory
}

// refuse tags a tool-level error with a core kind, so it gets that category,
// while keeping its own message.
func refuse(kind error, format string, args ...any) error {
	return &kindError{kind: kind, err: fmt.Errorf(format, args...)}
}

type kindError struct{ kind, err error }

func (e *kindError) Error() string   { return e.err.Error() }
func (e *kindError) Unwrap() []error { return []error{e.kind, e.err} }
