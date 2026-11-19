package portal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The address that portal-tunnel v2.6.3 gave the identity of testdata/identity-v2.6.3.json when it generated it
// (identity.Generate("blog") and identity.Marshal of that release).
const v263Address = "0xC9970bA44C3F85281406dd0b512915e003c28a09"

// An identity file that the Portal SDK of v2.6.3 wrote loads with the SDK of this build as the same identity: the same
// name, address and keys, so the public hostname of the flat does not change with the upgrade. The test writes the file as
// v2.6.3 did, with os.WriteFile and mode 0600, before the SDK sees it. On Windows it then checks the access list that the
// SDK gives the file (identityFileACL).
func TestIdentityWrittenByPortalV263Loads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "identity-v2.6.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		PublicKey  string `json:"public_key"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(data, &want); err != nil || want.PublicKey == "" || want.PrivateKey == "" {
		t.Fatalf("fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "blog.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	n, ff := newTestNet(t, Config{Dir: dir, Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	got := ff.last(t).id
	if got.Name != "blog" || got.Address != v263Address || got.PublicKey != want.PublicKey || got.PrivateKey != want.PrivateKey {
		t.Fatalf("identity = name %q address %q (public key equal %v, private key equal %v), want blog %s",
			got.Name, got.Address, got.PublicKey == want.PublicKey, got.PrivateKey == want.PrivateKey, v263Address)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
		t.Fatalf("the identity file changed: %v", err)
	}
	identityFileACL(t, path)
}
