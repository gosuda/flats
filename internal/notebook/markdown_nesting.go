package notebook

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// Goldmark counts both lists and list items as open blocks. Bound its stack
// before opening more containers; flattening only the resulting AST is too late.
const maxMarkdownParserDepth = 64

type markdownNestingParser struct {
	warnings *[]Warning
}

func (*markdownNestingParser) Trigger() []byte {
	// Free parsers run after character-triggered parsers regardless of priority.
	triggers := make([]byte, 256)
	for i := range triggers {
		triggers[i] = byte(i)
	}
	return triggers
}
func (*markdownNestingParser) CanInterruptParagraph() bool { return true }
func (*markdownNestingParser) CanAcceptIndentedLine() bool { return true }

func (p *markdownNestingParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	// A list's direct children must remain ListItems for Goldmark's continuation.
	if len(pc.OpenedBlocks()) < maxMarkdownParserDepth || parent.Kind() == ast.KindList {
		return nil, parser.NoChildren
	}
	line, _ := reader.PeekLine()
	at := 0
	for at < len(line) {
		for at < len(line) && (line[at] == ' ' || line[at] == '\t') {
			at++
		}
		if at < len(line) && line[at] == '>' {
			at++
			continue
		}
		end := at
		if end < len(line) && (line[end] == '-' || line[end] == '*' || line[end] == '+') {
			end++
		} else {
			for end < len(line) && line[end] >= '0' && line[end] <= '9' && end-at < 9 {
				end++
			}
			if end == at || end == len(line) || (line[end] != '.' && line[end] != ')') {
				break
			}
			end++
		}
		if end == len(line) || (line[end] != ' ' && line[end] != '\t' && line[end] != '\n') {
			break
		}
		at = end
	}
	lineNumber, _ := reader.Position()
	*p.warnings = append(*p.warnings, Warning{Code: "nesting_limited", Line: lineNumber + 1, Message: "Excess Markdown parser nesting becomes sibling text."})
	reader.Advance(at)
	_, segment := reader.PeekLine()
	node := ast.NewParagraph()
	node.Lines().Append(segment.TrimRightSpace(reader.Source()))
	reader.Advance(max(0, segment.Len()-1))
	return node, parser.NoChildren
}

func (*markdownNestingParser) Continue(ast.Node, text.Reader, parser.Context) parser.State {
	return parser.Close
}
func (*markdownNestingParser) Close(ast.Node, text.Reader, parser.Context) {}
