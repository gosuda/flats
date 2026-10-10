// Package runtimeref contains the agent documentation shipped with the host:
// guide topics and refusal pages under agent/, and the two versioned
// references assembled from them. The topics are the single source; the
// checked-in runtime-api-v1.md and content-types.md are generated from them
// (go test ./docs -run TestGeneratedReferences -update).
package runtimeref

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

const Version = "1"
const URI = "flats://docs/runtime-api/v1"

const ContentTypesVersion = "1"
const ContentTypesURI = "flats://docs/content-types/v1"

//go:embed agent/topics/*.md agent/references/*.md agent/refusals.md
var agentFS embed.FS

// Types is flats-runtime-v1.d.ts, the TypeScript declarations of runtime API
// v1 for JavaScript server flats. topic.server.types serves it after its
// introduction.
//
//go:embed agent/types/flats-runtime-v1.d.ts
var Types string

// TopicOrder lists every topic file under agent/topics in guide order.
var TopicOrder = []string{
	"index", "static", "design", "server", "server.types", "server.files", "server.db", "server.fetch", "server.limits",
	"docs", "manifest", "env-secrets", "approvals", "rollback-data", "preview-verify",
}

// Sections of each reference, in order, after its header.
var (
	runtimeSections      = []string{"static", "manifest", "server", "server.files", "server.db", "server.fetch", "server.limits", "server.types", "env-secrets", "preview-verify", "rollback-data", "approvals"}
	contentTypesSections = []string{"docs"}
)

var (
	// Markdown is the public, source-independent runtime API contract.
	Markdown string
	// ContentTypesMarkdown describes the host content adapters.
	ContentTypesMarkdown string
	// RefusalIntro explains refusal pages; it precedes them in guide output.
	RefusalIntro string

	topics     = map[string]string{}
	refusals   = map[string]string{}
	categories []string
)

func init() {
	files, err := fs.Glob(agentFS, "agent/topics/*.md")
	must(err)
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "agent/topics/"), ".md")
		if !slices.Contains(TopicOrder, name) {
			panic(fmt.Sprintf("runtimeref: topic %s is missing from TopicOrder", name))
		}
		topics[name] = read(f)
	}
	for _, name := range TopicOrder {
		if topics[name] == "" {
			panic(fmt.Sprintf("runtimeref: TopicOrder names missing topic %s", name))
		}
	}
	topics["server.types"] += "\n```ts\n" + Types + "```\n"
	Markdown = assemble("agent/references/runtime-api-v1.md", runtimeSections)
	ContentTypesMarkdown = assemble("agent/references/content-types.md", contentTypesSections)

	// refusals.md: an intro, then one "## <category>" section per category.
	parts := strings.Split(read("agent/refusals.md"), "\n## ")
	_, RefusalIntro, _ = strings.Cut(parts[0], "\n\n")
	RefusalIntro = strings.TrimSpace(RefusalIntro) + "\n"
	for _, p := range parts[1:] {
		name, _, _ := strings.Cut(p, "\n")
		if _, dup := refusals[name]; dup {
			panic("runtimeref: duplicate refusal " + name)
		}
		refusals[name] = "## " + strings.TrimRight(p, "\n") + "\n"
		categories = append(categories, name)
	}
}

func assemble(head string, sections []string) string {
	parts := []string{read(head)}
	for _, s := range sections {
		parts = append(parts, topics[s])
	}
	return strings.Join(parts, "\n")
}

func read(name string) string {
	b, err := agentFS.ReadFile(name)
	must(err)
	return string(b)
}

func must(err error) {
	if err != nil {
		panic("runtimeref: " + err.Error())
	}
}

// Topic returns the Markdown of topic.<name>.
func Topic(name string) (string, bool) {
	md, ok := topics[name]
	return md, ok
}

// Refusal returns the Markdown of refusal.<category>.
func Refusal(category string) (string, bool) {
	md, ok := refusals[category]
	return md, ok
}

// RefusalCategories lists every documented refusal category in file order.
func RefusalCategories() []string { return slices.Clone(categories) }
