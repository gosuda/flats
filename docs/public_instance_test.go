package runtimeref

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestPublicInstanceInstructionReplacements(t *testing.T) {
	for _, limit := range []int64{1000, 20 << 20} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			off := Instructions(limit)
			for i, r := range publicInstanceInstructionReplacements {
				for _, md := range []string{off, AutopilotInstructions(limit)} {
					if n := strings.Count(md, r.old); n != 1 {
						t.Errorf("anchor %q occurs %d times", r.old, n)
					}
				}
				start := strings.Index(off, r.old)
				for _, other := range publicInstanceInstructionReplacements[:i] {
					otherStart := strings.Index(off, other.old)
					if start < otherStart+len(other.old) && otherStart < start+len(r.old) {
						t.Errorf("public anchors overlap: %q and %q", r.old, other.old)
					}
				}
				for _, ap := range instructionReplacements {
					apStart := strings.Index(off, ap.old)
					if start < apStart+len(ap.old) && apStart < start+len(r.old) {
						t.Errorf("public anchor %q overlaps Autopilot anchor %q", r.old, ap.old)
					}
				}
			}
			for _, autopilot := range []bool{false, true} {
				for _, public := range []bool{false, true} {
					t.Run(fmt.Sprintf("autopilot=%t/public=%t", autopilot, public), func(t *testing.T) {
						md := ServerInstructions(limit, autopilot, public)
						if len(md) > 2300 {
							t.Errorf("instructions are %d bytes, limit 2300", len(md))
						}
						t.Logf("instructions: %d bytes", len(md))
						if !public {
							want := off
							if autopilot {
								want = AutopilotInstructions(limit)
							}
							if md != want {
								t.Fatal("mode-off instructions changed")
							}
							return
						}
						for _, r := range publicInstanceInstructionReplacements {
							if strings.Contains(md, r.old) || !strings.Contains(md, r.new) {
								t.Errorf("replacement not applied: %q", r.old)
							}
						}
						for _, want := range []string{"Public (view only)", "default public network", "topic.public-instance", "Private links and previews are disabled", "publishes Public flats created by this mode; older flats keep approvals"} {
							if !strings.Contains(md, want) {
								t.Errorf("missing %q", want)
							}
						}
						if autopilot != strings.Contains(md, "Autopilot is on") {
							t.Error("Autopilot instructions lost or leaked")
						}
					})
				}
			}
			// Applying either mode cannot consume the other's instruction anchors.
			publicFirst := replaceAll(off, publicInstanceInstructionReplacements)
			if replaceAll(publicFirst, instructionReplacements) != ServerInstructions(limit, true, true) {
				t.Error("instruction overlays are not disjoint")
			}
		})
	}
}

func TestPublicInstanceTopicReplacementsMatchOnce(t *testing.T) {
	for _, r := range publicInstanceTopicReplacements {
		t.Run(r.topic+"/"+r.old, func(t *testing.T) {
			md, ok := Topic(r.topic)
			if !ok {
				t.Fatalf("unknown topic %s", r.topic)
			}
			ap, _ := AutopilotTopicOverlay(r.topic)
			for _, text := range []string{md, ap} {
				if n := strings.Count(text, r.old); n != 1 {
					t.Errorf("anchor %q occurs %d times", r.old, n)
				}
			}
		})
	}
}

func TestPublicInstanceTopicOverlay(t *testing.T) {
	for _, autopilot := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot=%t", autopilot), func(t *testing.T) {
			for _, name := range TopicOrder {
				t.Run(name, func(t *testing.T) {
					off, _ := Topic(name)
					md := off
					if autopilot {
						md, _ = AutopilotTopicOverlay(name)
					}
					on := PublicInstanceTopicOverlay(name, md)
					changed := slices.ContainsFunc(publicInstanceTopicReplacements, func(r topicReplacement) bool { return r.topic == name })
					if changed == (on == md) {
						t.Errorf("replacement expected %t, topic changed %t", changed, on != md)
					}
					for _, r := range publicInstanceTopicReplacements {
						if r.topic == name && !strings.Contains(on, r.new) {
							t.Errorf("replacement missing: %q", r.new)
						}
					}
					baseline, _ := Topic(name)
					if baseline != off || strings.Contains(baseline, "topic.public-instance") {
						t.Error("baseline topic changed")
					}
				})
			}
			index, _ := Topic("index")
			if autopilot {
				index, _ = AutopilotTopicOverlay("index")
			}
			index = PublicInstanceTopicOverlay("index", index)
			routes, _, _ := strings.Cut(index, "### Workflow map")
			if !strings.Contains(index, "`topic.public-instance`") || strings.Contains(index, "runs Private") || strings.Contains(routes, "`open_preview`") {
				t.Errorf("index has stale routing:\n%s", index)
			}
			if autopilot && !strings.Contains(index, "`topic.autopilot`") {
				t.Error("public overlay lost the Autopilot route")
			}
		})
	}
	for _, name := range []string{"approvals", "index", "docs", "board", "canvas", "page", "static", "preview-verify", "preview-check"} {
		if !slices.ContainsFunc(publicInstanceTopicReplacements, func(r topicReplacement) bool { return r.topic == name }) {
			t.Errorf("required topic %s has no overlay", name)
		}
	}
	if got := PublicInstanceTopicOverlay("unknown", "unchanged"); got != "unchanged" {
		t.Errorf("unknown topic changed: %q", got)
	}
	for _, md := range []string{Markdown, ContentTypesMarkdown, Instructions(20 << 20)} {
		if strings.Contains(md, "public-instance") || strings.Contains(md, "public instance mode") {
			t.Error("mode leaked into a fixed reference or baseline instructions")
		}
	}
}

func TestPublicInstancePublicationGuidance(t *testing.T) {
	for _, autopilot := range []bool{false, true} {
		for _, name := range []string{"index", "page", "static", "design", "preview-verify", "preview-check"} {
			md, _ := Topic(name)
			if autopilot {
				md, _ = AutopilotTopicOverlay(name)
			}
			md = PublicInstanceTopicOverlay(name, md)
			if !strings.Contains(md, "created by this mode") || !strings.Contains(strings.ToLower(md), "older") {
				t.Errorf("%s (Autopilot %t) does not distinguish publication on save from older flats", name, autopilot)
			}
			for _, stale := range []string{"Saving never creates a version", "It has no `version` or `deploy` object", "pages stay Unpublished until approved"} {
				if strings.Contains(md, stale) {
					t.Errorf("%s retains %q", name, stale)
				}
			}
		}
	}
}

func TestPublicInstancePage(t *testing.T) {
	st := PublicInstanceState{Network: "proxy-public", Label: "Reverse proxy (Public)"}
	md := PublicInstancePage(st)
	if !strings.HasPrefix(md, "## Public instance mode\n") || strings.Contains(md, "{{") || strings.Contains(md, "## public_instance_on") {
		t.Fatalf("malformed page:\n%s", md)
	}
	for _, want := range []string{
		"Reverse proxy (Public) (`proxy-public`)", "take precedence", "llms.txt", "view only",
		"create_version", "deploy", "Private links", "open_preview", "HTTP 403", "Make Private",
		"creation is refused", "creates nothing", "not open yet", "public_notice", "Autopilot is independent",
		"Browser editing", "revision", "NOT access control",
		"Each save", "next numbered version", "without approval", "Older flats",
		"starter Draft", "not published", "keeps Draft", "without a new version",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if got := PublicInstancePage(PublicInstanceState{Network: "portal"}); !strings.Contains(got, "portal (`portal`)") {
		t.Error("missing label did not fall back to network id")
	}
	if got := PublicInstancePage(PublicInstanceState{}); !strings.Contains(got, "default public network is not configured.") {
		t.Error("missing default network not explained")
	}
}

func TestPublicInstanceRefusal(t *testing.T) {
	cats := PublicInstanceRefusalCategories()
	if !slices.Equal(cats, []string{"public_instance_on"}) {
		t.Fatalf("categories: %v", cats)
	}
	cats[0] = "changed"
	if PublicInstanceRefusalCategories()[0] != "public_instance_on" {
		t.Error("caller mutated category storage")
	}
	md, ok := PublicInstanceRefusal("public_instance_on")
	if !ok || !strings.HasPrefix(md, "## public_instance_on\n") || !strings.Contains(md, "previews are private") && !strings.Contains(md, "Previews are private") || !strings.Contains(md, "topic.public-instance") {
		t.Errorf("missing refusal: %q", md)
	}
	if md, ok := PublicInstanceRefusal("unknown"); ok || md != "" {
		t.Error("unknown refusal accepted")
	}
	if _, ok := Refusal("public_instance_on"); ok || slices.Contains(RefusalCategories(), "public_instance_on") {
		t.Error("category leaked into baseline refusal pages")
	}
	if _, ok := Topic("public-instance"); ok {
		t.Error("state-dependent topic leaked into baseline topics")
	}
}

func TestPublicInstanceGuideRevision(t *testing.T) {
	pi := &PublicInstanceState{Network: "portal", Label: "Portal"}
	ap := &AutopilotState{Guide: "Allow deployments."}
	for _, st := range []*AutopilotState{nil, ap} {
		t.Run(fmt.Sprintf("autopilot=%t", st != nil), func(t *testing.T) {
			off := EffectiveGuideRevision(1000, st)
			if off != EffectiveGuideRevision(1000, st, nil) {
				t.Fatal("explicit nil public state changed revision")
			}
			want := GuideRevision(1000)
			if st != nil {
				want = AutopilotGuideRevision(1000, *st)
			}
			if off != want {
				t.Fatal("mode-off revision changed")
			}
			rev := EffectiveGuideRevision(1000, st, pi)
			if len(rev) != 64 || rev != EffectiveGuideRevision(1000, st, pi) || rev == off {
				t.Fatal("public revision must be stable, distinct and SHA-256")
			}
			for name, other := range map[string]string{
				"limit":   EffectiveGuideRevision(2000, st, pi),
				"network": EffectiveGuideRevision(1000, st, &PublicInstanceState{Network: "zrok", Label: "Portal"}),
				"label":   EffectiveGuideRevision(1000, st, &PublicInstanceState{Network: "portal", Label: "Changed"}),
			} {
				if other == rev {
					t.Errorf("revision ignores %s", name)
				}
			}
		})
	}
	if EffectiveGuideRevision(1000, nil, pi) == EffectiveGuideRevision(1000, ap, pi) {
		t.Error("public revision ignores Autopilot")
	}
	left := &PublicInstanceState{Network: "ab", Label: "c"}
	right := &PublicInstanceState{Network: "a", Label: "bc"}
	if EffectiveGuideRevision(1000, nil, left) == EffectiveGuideRevision(1000, nil, right) {
		t.Error("network and label boundaries run together")
	}
}
