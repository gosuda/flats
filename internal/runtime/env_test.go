package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// RuntimeSpec.Env is the merged server-side map of ordinary app variables and
// decrypted secrets. Neither runtime should pick up the operator's environment.
func TestJSAppEnvironment(t *testing.T) {
	t.Setenv("FLATS_HOST_ONLY", "must-not-reach-app")
	configured := map[string]string{
		"MODE": "production", "EMPTY": "", "UNICODE": "서울 ☕\nsecond line",
		"API_KEY": "synthetic-secret-for-runtime-test",
		// Historical secret names remain readable. Native JS bindings retain
		// precedence over legacy DB/FILES strings without discarding other keys.
		"DB": "legacy-db-secret", "FILES": "legacy-files-secret",
		"PROTOTYPE": "legacy-prototype", "CONSTRUCTOR": "legacy-constructor",
		"__PROTO__": "legacy-proto", "LEGACY_NUL": "before\x00after",
	}
	f := mustStart(t, newManager(t), "js-env", map[string]string{"index.js": `
export default { fetch(request, env) {
  env.DB.exec("CREATE TABLE IF NOT EXISTS environment_test (value TEXT)");
  env.DB.exec("INSERT INTO environment_test VALUES (?)", env.UNICODE);
  env.FILES.put("environment-test", env.MODE);
  return Response.json({
    values: { MODE: env.MODE, EMPTY: env.EMPTY, UNICODE: env.UNICODE, API_KEY: env.API_KEY,
      PROTOTYPE: env.PROTOTYPE, CONSTRUCTOR: env.CONSTRUCTOR, __PROTO__: env.__PROTO__, LEGACY_NUL: env.LEGACY_NUL },
    keys: Object.keys(env).sort(),
    db: env.DB.query("SELECT value FROM environment_test")[0].value,
    file: env.FILES.get("environment-test"),
    hostMissing: env.FLATS_HOST_ONLY === undefined,
    processMissing: typeof process === "undefined"
  });
} }`}, "index.js", configured)
	var got struct {
		Values                      map[string]string
		Keys                        []string
		DB, File                    string
		HostMissing, ProcessMissing bool
	}
	f.json(t, "/", &got)
	expectedValues := make(map[string]string, len(configured)-2)
	for key, value := range configured {
		if key != "DB" && key != "FILES" {
			expectedValues[key] = value
		}
	}
	if !reflect.DeepEqual(got.Values, expectedValues) || !reflect.DeepEqual(got.Keys, []string{"API_KEY", "CONSTRUCTOR", "DB", "EMPTY", "FILES", "LEGACY_NUL", "MODE", "PROTOTYPE", "UNICODE", "__PROTO__"}) {
		t.Fatalf("injected environment = %+v", got)
	}
	if got.DB != configured["UNICODE"] || got.File != configured["MODE"] || !got.HostMissing || !got.ProcessMissing {
		t.Fatalf("bindings or host isolation = %+v", got)
	}
	if f.logs.find(configured["API_KEY"]) {
		t.Fatal("runtime logged the configured secret")
	}
}

func TestWASIAppEnvironment(t *testing.T) {
	t.Setenv("FLATS_HOST_ONLY", "must-not-reach-app")
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain to build a wasip1 guest")
	}
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"go.mod": "module guest\n\ngo 1.24\n",
		"main.go": `package main
import (
  "encoding/json"
  "os"
  "strings"
)
func main() {
  values := map[string]string{}
  for _, pair := range os.Environ() {
    key, value, _ := strings.Cut(pair, "=")
    values[key] = value
  }
  body, _ := json.Marshal(values)
  json.NewEncoder(os.Stdout).Encode(map[string]any{
    "status": 200, "headers": map[string]string{"content-type": "application/json"}, "body": string(body),
  })
}
`,
	})
	out := filepath.Join(t.TempDir(), "environment.wasm")
	cmd := exec.Command(goBin, "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot build a wasip1 guest: %v\n%s", err, b)
	}
	wasm, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	configured := map[string]string{
		"MODE": "production", "EMPTY": "", "UNICODE": "서울 ☕\nsecond=line",
		"API_KEY": "synthetic-secret-for-runtime-test",
		// WASI has no native DB/FILES environment bindings, so legacy names
		// must reach the guest unchanged, just like other historical secrets.
		"DB": "legacy-db-secret", "FILES": "legacy-files-secret",
		"PROTOTYPE": "legacy-prototype", "CONSTRUCTOR": "legacy-constructor",
		"__PROTO__": "legacy-proto",
	}
	f := mustStart(t, newManager(t), "wasi-env", map[string]string{"environment.wasm": string(wasm)}, "environment.wasm", configured)
	for i := 0; i < 2; i++ {
		var got map[string]string
		f.json(t, "/", &got)
		if !reflect.DeepEqual(got, configured) {
			t.Fatalf("WASI environment = %#v; want configured variables only", got)
		}
	}
	if strings.Contains(f.logs.String(), configured["API_KEY"]) {
		t.Fatal("runtime logged the configured secret")
	}
}
