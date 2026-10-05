# External API examples

See [setup, permissions and limits](../../docs/external-api.md).

- `server.js` and `flats.json`: deploy this directory as a server flat; needs an operator-granted origin,
  ordinary `API_BASE` and secret `API_TOKEN`. `/healthz` stays local.
- `browser.html`: copy to a separate directory as `index.html`, then deploy that
  directory as a static flat; calls a public CORS-enabled JSON
  endpoint. No server secret or environment variable goes into this file.

Deploy the server example with its checked-in manifest (the local health path
must stay `/healthz`):

```sh
flats deploy examples/external-api --flat hello
```

For a static browser flat, use a fresh directory containing only `index.html`
with the contents of `browser.html`. No manifest is needed for that default
static entry. Do not include this example's server `flats.json` in that directory.

Run the browser fixture with an existing Playwright installation:

```sh
PLAYWRIGHT_MODULE=playwright node examples/external-api/frontend_test.mjs
```

`PLAYWRIGHT_MODULE` may instead be an absolute path to an installed Playwright
package. `CHROMIUM_EXECUTABLE` optionally selects an existing Chromium binary.
The test starts two disposable loopback servers, uses an isolated browser
context with normal security settings, and closes all resources. No dependency
installation, operator data or external API credentials are required.
