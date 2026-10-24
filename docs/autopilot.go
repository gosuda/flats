package runtimeref

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strings"
)

// The texts agents read while Autopilot is on. Each is today's text with
// exact replacements (a test requires every old text to occur exactly once),
// plus topic.autopilot and its refusal pages from agent/autopilot.md. Off
// texts and the fixed references never change. Nothing here keeps state:
// every function is pure over its arguments and the embedded files.

//go:embed agent/autopilot.md
var autopilotMarkdown string

// AutopilotOperation is one operation agents may decide.
type AutopilotOperation struct {
	ID       string // "deploy"
	Label    string // "Deploy"
	Requests string // "deploy of a saved version (activation or redeploy)"
}

// AutopilotState is what the on texts depend on. A nil *AutopilotState
// means Autopilot is off.
type AutopilotState struct {
	Allowed []AutopilotOperation // in operation order; empty when none
	Guide   string
}

type replacement struct{ old, new string }

// instructionReplacements turn Instructions into AutopilotInstructions.
var instructionReplacements = []replacement{
	{"wait for explicit operator approval and change nothing before it.", "wait for an approval and change nothing before it."},
	{"Give the user approval_url exactly as returned and poll get_approval. Never approve your own request or imitate the console; MCP has no approval tool.",
		"Autopilot is on: approve_request and reject_request decide approvals of operations the operator allows; read guide topic.autopilot first. Otherwise give the user approval_url exactly as returned and poll get_approval. Never imitate the console."},
	{"- Ask the user before set_visibility, delete_flat or rollback with restore_data.\n", ""},
}

type topicReplacement struct {
	topic string
	replacement
}

// topicReplacements give the on text of guide topics.
var topicReplacements = []topicReplacement{
	{"approvals", replacement{"Agents never decide publication, exposure or deletion.",
		"Autopilot is on: agents may decide the approvals `topic.autopilot` lists, and the operator decides the others."}},
	{"approvals", replacement{"until the operator approves it in the Flats console:", "until it is approved:"}},
	{"approvals", replacement{"say you are waiting for operator approval.", "say you are waiting for approval."}},
	{"approvals", replacement{"* Never approve your own request, call console routes or send console\n  headers. MCP has no approval or provider-grant tool.",
		"* Decide approvals only with `approve_request` and `reject_request`\n  (`topic.autopilot`). Never call console routes or send console\n  headers. MCP has no provider-grant tool."}},
	{"approvals", replacement{"* Ask the user before requesting `set_visibility`, `delete_flat` or a rollback\n  with `restore_data`.",
		"* Whether to ask a person before `set_visibility`, `delete_flat` or a\n  rollback with `restore_data` is up to the operation guide in\n  `topic.autopilot` and your own policy."}},
	{"rollback-data", replacement{"Nothing is restored before operator approval.\nAsk the user before requesting it, then poll",
		"Nothing is restored before approval\n(`topic.autopilot`). After requesting it, poll"}},
	{"index", replacement{"| `set_visibility`, `delete_flat` (ask the user first) |", "| `set_visibility`, `delete_flat` |"}},
	{"index", replacement{"| Understand a refused call |",
		"| Decide pending approvals while Autopilot is on | `topic.autopilot` | `approve_request`, `reject_request` |\n| Understand a refused call |"}},
	{"index", replacement{"`delete_flat` also wait for the operator (`topic.approvals`).", "`delete_flat` also wait for approval (`topic.approvals`, `topic.autopilot`)."}},
	{"board", replacement{"deleting it waits for the operator (`topic.approvals`).", "deleting it waits for approval (`topic.approvals`)."}},
	{"board", replacement{"It waits for the operator's\n  approval like every visibility change", "It waits for\n  approval like every visibility change"}},
	{"docs", replacement{"wait for the operator's approval (`topic.approvals`).", "wait for approval (`topic.approvals`)."}},
	{"docs", replacement{"It waits for the operator's\n  approval like every visibility change", "It waits for\n  approval like every visibility change"}},
	{"static", replacement{"requests operator approval; share its", "requests approval; share its"}},
	{"client-update", replacement{"never replace the operator approval flow", "never replace the approval flow"}},
}

func replaceAll(text string, reps []replacement) string {
	for _, r := range reps {
		text = strings.Replace(text, r.old, r.new, 1)
	}
	return text
}

var (
	autopilotTopic    string // topic.autopilot before its state is filled in
	autopilotRefusals = map[string]string{}
	autopilotCats     []string
)

func init() {
	// agent/autopilot.md: the topic, then one "## <category>" section per
	// refusal category.
	parts := strings.Split(autopilotMarkdown, "\n## ")
	autopilotTopic = parts[0]
	for _, p := range parts[1:] {
		name, _, _ := strings.Cut(p, "\n")
		if _, dup := autopilotRefusals[name]; dup {
			panic("runtimeref: duplicate autopilot refusal " + name)
		}
		autopilotRefusals[name] = "## " + strings.TrimRight(p, "\n") + "\n"
		autopilotCats = append(autopilotCats, name)
	}
}

// AutopilotInstructions are the server instructions while Autopilot is on.
func AutopilotInstructions(uploadLimit int64) string {
	return replaceAll(Instructions(uploadLimit), instructionReplacements)
}

// AutopilotTopicOverlay returns the on text of the existing topic.<name>:
// its replacements applied, or the topic unchanged when it has none.
func AutopilotTopicOverlay(name string) (string, bool) {
	md, ok := Topic(name)
	if !ok {
		return "", false
	}
	for _, r := range topicReplacements {
		if r.topic == name {
			md = strings.Replace(md, r.old, r.new, 1)
		}
	}
	return md, true
}

// AutopilotPage renders topic.autopilot: the rules, the operations allowed
// in st and the operation guide.
func AutopilotPage(st AutopilotState) string {
	var ops strings.Builder
	if len(st.Allowed) == 0 {
		ops.WriteString("None: the operator allows no operation yet; give the user `approval_url` for every approval.\n")
	}
	for _, op := range st.Allowed {
		fmt.Fprintf(&ops, "* %s (`%s`): %s\n", op.Label, op.ID, op.Requests)
	}
	md := strings.Replace(autopilotTopic, "{{operations}}\n", ops.String(), 1)
	return strings.Replace(md, "{{guide}}\n", fencedGuide(st.Guide), 1)
}

// fencedGuide puts the operator's guide in a fence one backtick longer than
// its longest backtick run, so the guide cannot close the fence or add
// headings.
func fencedGuide(guide string) string {
	if strings.TrimSpace(guide) == "" {
		return "The operator wrote no operation guide.\n"
	}
	longest, run := 0, 0
	for _, c := range guide {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + strings.TrimSuffix(guide, "\n") + "\n" + fence + "\n"
}

// AutopilotRefusal returns the Markdown of refusal.<category> for the
// categories that exist only while Autopilot is on.
func AutopilotRefusal(category string) (string, bool) {
	md, ok := autopilotRefusals[category]
	return md, ok
}

// AutopilotRefusalCategories lists the Autopilot refusal categories in file
// order.
func AutopilotRefusalCategories() []string { return append([]string(nil), autopilotCats...) }

// AutopilotGuideRevision identifies the guide an agent reads while Autopilot
// is on with st: the off guide, the Autopilot texts and replacements, and
// the allowed operations and operation guide.
func AutopilotGuideRevision(uploadLimit int64, st AutopilotState) string {
	h := sha256.New()
	add := func(name, body string) { fmt.Fprintf(h, "%d:%s%d:%s", len(name), name, len(body), body) }
	add("guide", GuideRevision(uploadLimit))
	add("autopilot", autopilotMarkdown)
	for _, r := range instructionReplacements {
		add("instructions.old", r.old)
		add("instructions.new", r.new)
	}
	for _, r := range topicReplacements {
		add(r.topic+".old", r.old)
		add(r.topic+".new", r.new)
	}
	for _, op := range st.Allowed {
		add("allowed", op.ID)
	}
	add("operation-guide", st.Guide)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// EffectiveGuideRevision is GuideRevision while Autopilot is off (st nil)
// and AutopilotGuideRevision while it is on.
func EffectiveGuideRevision(uploadLimit int64, st *AutopilotState) string {
	if st == nil {
		return GuideRevision(uploadLimit)
	}
	return AutopilotGuideRevision(uploadLimit, *st)
}
