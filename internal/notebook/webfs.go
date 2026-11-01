package notebook

import "embed"

// webFS holds the browser client: the shell, its modules and styles. Tests
// under web/testdata are not embedded.
//
//go:embed web/index.html web/*.js web/*.css
var webFS embed.FS
