package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWASIRejectsJSHostABI(t *testing.T) {
	for _, capability := range []string{"DB", "FILES", "websocket"} {
		t.Run(capability, func(t *testing.T) {
			// Minimal valid module importing one () -> () function from "flats".
			module := []byte{'\x00', 'a', 's', 'm', 1, 0, 0, 0, 1, 4, 1, 0x60, 0, 0}
			imp := []byte{1, 5, 'f', 'l', 'a', 't', 's', byte(len(capability))}
			imp = append(imp, capability...)
			imp = append(imp, 0, 0)
			module = append(module, 2, byte(len(imp)))
			module = append(module, imp...)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "guest.wasm"), module, 0o600); err != nil {
				t.Fatal(err)
			}
			w := &worker{spec: workerSpec{Dir: dir, Entry: "guest.wasm"}}
			e, err := newWASIEngine(w)
			if err == nil {
				e.close()
				t.Fatal("WASI accepted a JS host capability import")
			}
			if !strings.Contains(err.Error(), `only wasi_snapshot_preview1 is available`) {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}
