package sheet

import "embed"

// webFS holds the browser client: the shell, its modules and styles. Tests
// under web/testdata are not embedded. functions.json is not read from
// here: the instance generates it from the formula registry.
//
//go:embed web/index.html web/*.js web/*.css
var webFS embed.FS
