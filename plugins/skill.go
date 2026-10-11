// Package plugins embeds the bootstrap shipped by the Flats plugin.
package plugins

import _ "embed"

// Skill is the single source used by both installed plugins and the server.
//
//go:embed flats/skills/flats-deploy/SKILL.md
var Skill string
