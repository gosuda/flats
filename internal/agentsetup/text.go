// Package agentsetup edits the instruction and settings files of coding
// agents so that documents go to Flats. It keeps no state: everything it adds
// can be recognized in the file it changed. See docs/spec/agent-document-routing.md.
package agentsetup

import "strings"

// BeginMarker and EndMarker delimit the block this package owns in an
// instruction file. A marker counts only as a whole line outside a fenced code
// block.
const (
	BeginMarker = "<!-- flats:documents:begin -->"
	EndMarker   = "<!-- flats:documents:end -->"
)

// RoutingText is the one text rendered into every instruction target.
const RoutingText = "## Documents\n" +
	"\n" +
	"When I ask for a document that people will read, share or edit afterwards, such as a report, a proposal, a plan or notes, create it in Flats with the Flats MCP tools. Do not use another document tool or connector for it.\n" +
	"\n" +
	"Start with `check_client`, then call `guide` for `topic.docs`. Create the document with `create_flat` (type `docs`) and write it with `save_document`.\n" +
	"\n" +
	"If the Flats tools are not available, tell me instead of using another document tool.\n"

// Claude Code settings entries that remove the competing Claude Docs route.
const (
	ClaudeDocsServer = "claude.ai Claude Docs"
	DenySkill        = "Skill(anthropic-skills:docs)"
	DenyTools        = "mcp__claude_ai_Claude_Docs__*"
)

// cursorFrontMatter opens a Cursor rule file that applies to every request.
const cursorFrontMatter = "---\n" +
	"description: Create documents in Flats\n" +
	"alwaysApply: true\n" +
	"---\n"

// blockLines is the begin marker, the routing text and the end marker.
func blockLines() []string {
	text := strings.Split(strings.TrimRight(RoutingText, "\n"), "\n")
	lines := make([]string, 0, len(text)+2)
	lines = append(lines, BeginMarker)
	lines = append(lines, text...)
	return append(lines, EndMarker)
}

// CursorRule is the content of a new Cursor rule file.
func CursorRule() []byte {
	out, _, _ := SetBlock([]byte(cursorFrontMatter))
	return out
}
