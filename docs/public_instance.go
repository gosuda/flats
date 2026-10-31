package runtimeref

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed agent/public-instance.md
var publicInstanceMarkdown string

// PublicInstanceState is the default public network while the mode is on.
// A nil *PublicInstanceState means the mode is off.
type PublicInstanceState struct {
	Network string
	Label   string
}

var publicInstanceInstructionReplacements = []replacement{
	{" and a Private URL through Local loopback or an explicitly permitted Tailscale, NetBird or reverse proxy route.",
		". New flats start Public (view only) on the default public network. Public instance mode is on: read topic.public-instance."},
	{"only changes a Private Draft.", "publishes Public flats created by this mode; older flats keep approvals."},
	{"Creating or deleting a version, activation, rollback", "Explicit version creation/deletion, deploy, rollback"},
	{"it is Private unless the operator approved Public, where everyone sees changes immediately.",
		"new flats start Public (view only)."},
	{"Private follows loopback, the tailnet or NetBird access rules, or the operator's reverse proxy; it is not owner-only.",
		"Private links and previews are disabled."},
}

var publicInstanceTopicReplacements = []topicReplacement{
	{"index", replacement{"(Draft, then approved publish)",
		"(new Public pages publish on save; older pages keep approvals)"}},
	{"index", replacement{"3. **Save version.** `create_version` returns",
		"3. **Other flats.** Public websites and pages created by this mode already\n   published on save; skip to Verify. Otherwise `create_version` returns"}},
	{"index", replacement{"4. **Deploy.** Call `deploy {slug, version: N}`",
		"4. **Other flats.** Call `deploy {slug, version: N}`"}},
	{"page", replacement{"4. `create_version` requests approval to save the",
		"4. Public pages created by this mode already published on save. Otherwise\n   `create_version` requests approval to save the"}},
	{"static", replacement{"Saving never creates a version or approval. Call `create_version {slug,\nrevision, hash}` separately to request approval to freeze the Draft as a numbered\nversion (`v1`, `v2`, ...). After approval creates `vN`, call `deploy {slug,\nversion: N}` and obtain another approval to make it live (`topic.approvals`).",
		"Saving Public flats created by this mode publishes the next version without\napproval. Older or Private flats only change the Draft; request `create_version {slug,\nrevision, hash}` and `deploy {slug, version: N}` through their approvals\n(`topic.approvals`)."}},
	{"static", replacement{"`flats save-draft <slug> <dir>`, then `flats create-version <slug>` and its\napproval, then `flats deploy --flat <slug> --version <n>` and another approval.",
		"`flats save-draft <slug> <dir>`, which publishes Public flats created by this\nmode. Older flats then need `flats create-version <slug>` and its approval,\nthen `flats deploy --flat <slug> --version <n>` and another approval."}},
	{"static", replacement{"later. `saved_at` is this save's time. It has no `version` or `deploy` object.",
		"later. `saved_at` is this save's time. A published save also returns its\npublished version and flat metadata; older flats return only the Draft."}},

	{"approvals", replacement{"New flats are Unpublished and\n  Private.",
		"New flats start Public (view only) on the default public network\n  without a visibility approval (`topic.public-instance`). Saving a Public website or\n  page created by this mode publishes it. Older or Private flats keep approvals."}},
	{"approvals", replacement{"**Private** means Local loopback on the host (plain HTTP at\n  `<slug>.localhost:<port>`), Tailscale for people and devices the existing\n  tailnet ACL allows, NetBird for peers the NetBird access policies allow, or\n  the operator's reverse proxy (`proxy`) for whoever that proxy lets through.\n  It is not owner-only access.",
		"**Private** flats have no working link while public instance mode is on.\n  All private routes, Local included, are disabled (`topic.public-instance`)."}},
	{"approvals", replacement{"An Unpublished flat cannot become Public. Requesting the current\n  visibility changes nothing.",
		"An Unpublished flat still cannot become Public through a visibility\n  approval. Public instance creation is the exception: new websites and\n  pages are Public and Unpublished. Requesting the current visibility changes nothing."}},
	{"approvals", replacement{"Going Private also waits for approval. Until the approved change applies,\n  the public route stays reachable.",
		"Going Private also waits for approval. Until the approved change applies,\n  the public route stays reachable. Afterward the flat has no working link\n  while public instance mode is on."}},
	{"approvals", replacement{"It is\n  still created Private: a public default never publishes it or changes\n  visibility, which still needs approval.",
		"Public instance mode overrides those creation defaults: a new flat is\n  Public (view only), with only the default public network granted and no\n  private network grant. Creation needs no visibility approval; later\n  visibility changes still do. Unusable defaults refuse creation rather\n  than falling back (`topic.public-instance`)."}},
	{"approvals", replacement{"`endpoints` lists every route of a flat, Local included. `private_url` and\n  `public_url` name the route on the host's default network when the flat has\n  one there, otherwise the first by a fixed order. Give the user those URLs,\n  and the Local one only for use on the Flats host itself.",
		"`endpoints` lists only public routes while public instance mode is on.\n  `private_url` is empty (HTTP flat views also set `private_links_off`). Give the user\n  `public_url` with `public_notice` only after checking readiness; if it is\n  absent or not ready, report that the public link is not open yet and why."}},
	{"index", replacement{"| Understand a refused call |",
		"| Work while new flats start Public and private links are disabled | `topic.public-instance` | `get_flat` |\n| Understand a refused call |"}},
	{"index", replacement{"(runs Private at once; no publish or approval).", "(starts Public, view only; runs at once without creation approval)."}},
	{"index", replacement{"(runs Private at once; no publish or approval) | `topic.board`", "(starts Public, view only; runs at once without creation approval) | `topic.board`"}},
	{"index", replacement{"(runs Private at once; no publish or approval) | `topic.docs`", "(starts Public, view only; runs at once without creation approval) | `topic.docs`"}},
	{"index", replacement{"`save_draft`, `open_preview`, `create_version`, `deploy`", "`save_draft`, `create_version`, `deploy`"}},
	{"index", replacement{"`save_draft` warnings, `open_preview`", "`save_draft` warnings, deployed public URL"}},
	{"index", replacement{"`save_page`, `open_preview`, `create_version`, `deploy`", "`save_page`, `create_version`, `deploy`"}},
	{"index", replacement{"| Check a Draft or a published version | `topic.preview-verify` | `open_preview`, `get_flat`, `get_logs` |",
		"| Inspect a Draft or verify a deployed version (no previews) | `topic.preview-verify` | `get_draft`, `get_flat`, `get_logs` |"}},
	{"index", replacement{"| Look at a rendered preview in your browser (overflow, contrast, failed loads, thumbnail) | `topic.preview-check` | your browser tool, `open_preview` |",
		"| Check the deployed public page in your browser (no Draft previews) | `topic.preview-check` | your browser tool, `get_flat` |"}},
	{"index", replacement{"saving creates a missing flat and only changes its Private Draft. Nothing\n   is published.",
		"saving creates a missing flat Public (view only) and publishes it. Each\n   save of a Public website or page created by this mode stores and deploys\n   the next version without approval. Older or Private flats keep approvals."}},
	{"index", replacement{"2. **Review.** Read the save `warnings`, then `open_preview {slug, version:\n   0}` serves the Draft privately; look once in your browser and fix\n   (`topic.design`, `topic.preview-check`).",
		"2. **Review.** Read save `warnings` and inspect the files. Private links\n   and `open_preview` are disabled; there is no public Draft preview.\n   Check the deployed public link after a published save, or after approved\n   deployment for older flats (`topic.public-instance`, `topic.preview-check`)."}},
	{"docs", replacement{"A Private link opens the editor and can toggle between Edit\nand View; a Public link is view only (a read-only rendered document), unless",
		"Private links are disabled.\nA Public link is view only (a read-only rendered document), unless"}},
	{"docs", replacement{"Creating or saving a document runs it at once, Private, with no\napproval, and its Private URL serves it as soon as the host is ready.",
		"Creating or saving a document runs it at once without approval. A new\ndocument starts Public (view only) on the default public network\n(`topic.public-instance`); later saves keep its visibility. Its public\nlink serves it when ready. On a Public document every save is visible\nto everyone."}},
	{"docs", replacement{"Report its\n   `private_url` once `get_flat` shows `private_state: ready`.",
		"Report its\n   `public_url` with `public_notice` once `get_flat` shows a ready public\n   endpoint. Otherwise report that the public link is not open yet and why."}},
	{"docs", replacement{"the way a person edits in the Private editor:", "without using the disabled Private editor:"}},
	{"docs", replacement{"People see the same history in the document's History panel on its\nPrivate link.",
		"Browser history is unavailable while private links are disabled; use\n`list_revisions` and `get_revision` through MCP instead."}},
	{"docs", replacement{"the link reads; only Private routes edit)", "the link reads; private editing routes are disabled)"}},
	{"board", replacement{"A new board is Private.",
		"A new board starts Public (view only) on the default public network\n(`topic.public-instance`), without a visibility approval."}},
	{"board", replacement{"the user `private_url`: it opens the canvas.",
		"the user `public_url` with `public_notice` once the public endpoint is\n   ready. Otherwise report that the public link is not open yet and why."}},
	{"board", replacement{"link watches; only Private routes edit)", "link watches; private editing routes are disabled)"}},
	{"board", replacement{"Give the user `private_url`. If you have a browser tool, open the canvas\n   and look at the changed area.",
		"Give the user the ready `public_url` with `public_notice`. If you have a\n   browser tool, open that view-only link and look at the changed area.\n   Private links and browser history are disabled; MCP revision tools work."}},
	{"canvas", replacement{"It is Private unless the operator approved Public.",
		"A new design starts Public (view only) on the default public network\n(`topic.public-instance`), without a visibility approval. Later saves keep\nits visibility; private links and browser editing are disabled."}},
	{"canvas", replacement{"the user `private_url`: it opens the canvas.",
		"the user the ready `public_url` with `public_notice`: it opens the\n   view-only canvas. Otherwise report that the public link is not open yet\n   and why. Browser editing and history are disabled; MCP tools still work."}},
	{"canvas", replacement{"**Opening a file directly** at `/<path>` on a Private route works for\n  viewing, but cross-origin loads such as `fetch` of bundled data or web\n  fonts work only inside the canvas.",
		"**Opening a file directly** through a Private route is disabled. Use\n  the public canvas for viewing; cross-origin loads such as `fetch` of\n  bundled data or web fonts work only inside the canvas."}},
	{"canvas", replacement{"People change layout, notes, text and tweaks in the browser while you work.",
		"Private browser editing is disabled while public instance mode is on.\nPreviously prepared browser edits can still conflict with your work."}},
	{"canvas", replacement{"Give the user `private_url`.", "Give the user the ready `public_url` with `public_notice`."}},
	{"page", replacement{"only a Private Draft, and the page serves after an operator-approved publish",
		"a Draft and publish it for Public pages created by this mode. Older\nor Private pages keep approved version creation and deployment"}},
	{"page", replacement{"3. Read the save `warnings`, then `open_preview {slug, version: 0}` and look\n   at it in your browser (`topic.preview-check`).",
		"3. Read save `warnings` and inspect the files. `open_preview` is disabled;\n   there is no public Draft preview. Check the deployed public link in your\n   browser after publication (`topic.preview-check`, `topic.public-instance`)."}},
	{"static", replacement{"A missing flat is created on the first save.",
		"A missing flat is created Public (view only) on the default public\nnetwork on the first save, which publishes it (`topic.public-instance`).\nEach save of a Public flat created by this mode publishes the next version\nwithout approval. Older or Private flats keep approvals. Private links and\n`open_preview` are disabled; inspect files before saving, then check save\nwarnings and verify the deployed public link."}},
	{"preview-verify", replacement{"### Preview\n\nBefore previewing, read the `warnings` of the save result (`topic.design`);\nthey are cheap static checks, not a render.\n\n`open_preview {slug, version: 0}` serves the current Private Draft; a positive\n`version` previews that published version. Live is unchanged. Documents have\nno previews: they run at once, so open their private URL instead. Server\nflats get an isolated copy of the live DB/FILES: preview writes never reach\nlive. New previews capture current environment variables, secrets and network\ngrants. At most **5 open previews per flat**; a preview closes on the next\nactivation of that flat, or after the operator setting `preview_ttl_seconds`\n(default **24 hours**) without visits. Previews are Private (loopback, the\nexisting tailnet ACL or the operator's reverse proxy) and never use Public\nproviders.",
		"### No previews while public instance mode is on\n\n`open_preview` is refused (`refusal.public_instance_on`). Private links are\ndisabled, including existing Draft and version previews. There is no public\nDraft preview. Inspect the saved files and save `warnings` (`topic.design`);\nthese are static checks, not a render. Saving Public websites and pages\ncreated by this mode publishes them; older or Private flats still need approved\n`create_version` and `deploy` before browser verification at the public URL.\nDocuments, designs and boards run at once; use their ready public URL with\n`public_notice`, never a private URL (`topic.public-instance`)."}},
	{"preview-verify", replacement{"A URL can exist before it answers. `get_flat` reports `private_state` and each\npreview's `state`: `ready`, or `starting` while a tailnet node joins or waits\nfor its HTTPS certificate (often 1-2 minutes for a new flat or preview). Check\n`get_flat` again before fetching; a TLS error while `starting` is not a failed\ndeploy. Local mode serves plain HTTP at `<slug>.localhost:<local-port>`. To\nverify a local-mode URL from another network namespace, connect to the host's\naddress and port with the returned URL's Host header; that is not public\nexposure.\n\nGive the user the URLs that `get_flat` and `open_preview` return; never build\none from a pattern. A Local link (`*.localhost`) opens on the machine where it\nis clicked, so it reaches the flat only from the Flats host itself. On a host\nwhose private backend is Tailscale, new flats and Draft previews use the\ntailnet address, which other allowed devices can open.",
		"A URL can exist before it answers. Check `get_flat` for `public_url`,\n`connection_state` and a ready public endpoint, then fetch it. Never build\na URL from a pattern. Repeat `public_notice` whenever you share it. If the\npublic link is absent or not ready, report that it is not open yet and why.\nNew websites and pages are Public and Unpublished with no link until first\ndeployment. Private flats have no working link while the mode is on.\nNever fall back to Local, a tailnet or another private provider."}},
	{"preview-verify", replacement{"A reverse proxy route (`proxy` or `proxy-public`)", "A public reverse proxy route (`proxy-public`)"}},
	{"preview-verify", replacement{"For a page, check the Draft preview once in your own browser at desktop\nand phone width, in light and dark, with the check script\n(`topic.preview-check`); fix what you find in one pass, then publish\n(`topic.design`). Do not repeat the look in a loop. Flats itself does not\nrender pages.",
		"For a page, check the deployed public URL once in your own browser at\ndesktop and phone width, in light and dark, with the check script\n(`topic.preview-check`). Corrections publish on the next save for Public\npages created by this mode; older or Private flats need version creation and deployment\nthrough approvals. Do not claim a pre-publication render check: there are\nno previews. Flats itself does not render pages."}},
	{"preview-check", replacement{"## Check a preview in your browser", "## Check the deployed public page in your browser"}},
	{"preview-check", replacement{"Draft preview with it once before you publish.",
		"deployed public page with it once. Private links and `open_preview` are\ndisabled: there is no public Draft preview (`topic.public-instance`)."}},
	{"preview-check", replacement{"* Open the URL that `open_preview {slug, version: 0}` returned, after\n  `get_flat` reports that preview `ready` (`topic.preview-verify`). Never\n  build the URL yourself.\n* Your browser must reach it. A Local preview (`*.localhost`) opens only on\n  the Flats host itself; a Tailscale preview opens on devices the tailnet ACL\n  allows, and a reverse proxy preview for whoever the operator's proxy lets\n  through. If your browser cannot reach the preview, say so; do not report a\n  visual check you did not make.",
		"* Save Public websites and pages created by this mode to publish them;\n  older or Private flats need approved `create_version` and `deploy`. Open `public_url`\n  only after `get_flat` reports the intended live version\n  with a ready public endpoint (`topic.preview-verify`). Repeat\n  `public_notice` when sharing the URL. Never build the URL yourself.\n* If your browser cannot reach the public link, say so; do not fall back to\n  a private link or report a visual check you did not make."}},
	{"preview-check", replacement{"Then fix everything you found in one pass and add the thumbnail (below) in\nthe same change, save the Draft once more and `create_version` (`topic.approvals`):\npublishing freezes the saved Draft, so anything added after it is not in that\nversion.",
		"Then fix everything you found in one pass and add the thumbnail (below)\nin the same change. For Public flats created by this mode, saving publishes\nthe fixes as the next version. Older or Private flats need `create_version` and\n`deploy` through approvals (`topic.approvals`). The first public version\nwas already visible during this check."}},
	{"design", replacement{"flat's thumbnail (`topic.manifest`). Capture it from the Draft preview at\n  desktop width; keep it small (PNG or WebP, well under 500 KiB).",
		"flat's thumbnail (`topic.manifest`). With private previews disabled,\n  capture it from the deployed public page at desktop width and include it\n  in the next save. Public flats created by this mode publish it at once;\n  older flats keep approvals. Keep it small (PNG or WebP, well under 500 KiB)."}},
	{"design", replacement{"2. `open_preview {slug, version: 0}` and wait until its `state` is `ready`\n   (`topic.preview-verify`).\n3. With a browser tool, check the preview once in four views (desktop and\n   phone width, light and dark) and run the check script",
		"2. Private links and `open_preview` are disabled. Public websites and pages\n   created by this mode publish on save; older or Private flats need `create_version`\n   and `deploy` through approvals. Verify the deployed public URL when ready\n   (`topic.public-instance`, `topic.preview-verify`).\n3. With a browser tool, check the deployed page once in four views (desktop\n   and phone width, light and dark) and run the check script"}},
	{"index", replacement{"on a canvas people edit with you", "on a view-only public canvas"}},
	{"index", replacement{"connectors that people edit with you", "connectors on a public board (guest editing needs approval)"}},
	{"canvas", replacement{"People open it in a browser, see the artboards side by side, play the\nprototype, and move, retype and tweak things in place.",
		"People open its Public link in a browser, see the artboards side by\nside and play the prototype. Moving, retyping and tweaking in the browser\nrequire Private links and are disabled while public instance mode is on."}},
	{"canvas", replacement{"People watching the canvas see it fill.",
		"Public viewers see the canvas fill. Repeat `public_notice`."}},
	{"design", replacement{"### Check once, then publish", "### Publish, then check once"}},
	{"design", replacement{"4. Make one pass of fixes, save again, then `create_version` (`topic.approvals`).",
		"4. Make one pass of fixes and save again. Public websites and pages created\n   by this mode publish on save. Older or Private flats need `create_version` and\n   `deploy` through approvals (`topic.approvals`)."}},
}

var (
	publicInstanceTopic   string
	publicInstanceRefusal string
)

func init() {
	var ok bool
	publicInstanceTopic, publicInstanceRefusal, ok = strings.Cut(publicInstanceMarkdown, "\n## public_instance_on\n")
	if !ok {
		panic("runtimeref: missing public_instance_on refusal")
	}
	publicInstanceRefusal = "## public_instance_on\n" + publicInstanceRefusal
}

// ServerInstructions applies Autopilot first, then public instance mode.
// The mode-off variants remain identical to the original instructions.
func ServerInstructions(uploadLimit int64, autopilot, public bool) string {
	md := Instructions(uploadLimit)
	if autopilot {
		md = replaceAll(md, instructionReplacements)
	}
	if public {
		md = replaceAll(md, publicInstanceInstructionReplacements)
	}
	return md
}

// PublicInstanceTopicOverlay applies to a topic after its Autopilot overlay.
// It does not change the embedded baseline topics or fixed references.
func PublicInstanceTopicOverlay(name, md string) string {
	for _, r := range publicInstanceTopicReplacements {
		if r.topic == name {
			md = strings.Replace(md, r.old, r.new, 1)
		}
	}
	return md
}

// PublicInstancePage renders topic.public-instance for the current network.
func PublicInstancePage(st PublicInstanceState) string {
	network := "not configured"
	if st.Network != "" {
		label := st.Label
		if label == "" {
			label = st.Network
		}
		network = fmt.Sprintf("%s (`%s`)", label, st.Network)
	}
	return strings.Replace(publicInstanceTopic, "{{network}}", network, 1)
}

// PublicInstanceRefusal returns the category available only in this mode.
func PublicInstanceRefusal(category string) (string, bool) {
	if category == "public_instance_on" {
		return publicInstanceRefusal, true
	}
	return "", false
}

// PublicInstanceRefusalCategories lists the mode's refusal categories.
func PublicInstanceRefusalCategories() []string { return []string{"public_instance_on"} }

func publicInstanceGuideRevision(base string, st PublicInstanceState) string {
	h := sha256.New()
	add := func(name, body string) { fmt.Fprintf(h, "%d:%s%d:%s", len(name), name, len(body), body) }
	add("guide", base)
	add("public-instance", publicInstanceMarkdown)
	for _, r := range publicInstanceInstructionReplacements {
		add("instructions.old", r.old)
		add("instructions.new", r.new)
	}
	for _, r := range publicInstanceTopicReplacements {
		add(r.topic+".old", r.old)
		add(r.topic+".new", r.new)
	}
	add("network", st.Network)
	add("label", st.Label)
	return fmt.Sprintf("%x", h.Sum(nil))
}
