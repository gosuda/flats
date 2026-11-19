package portal

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// identityFileACL checks the access list that the Portal SDK gives an identity file on Windows (Windows support spec,
// rule 18): protected, with full access for SYSTEM, the Administrators group and the current user, and nothing else.
// The SDK leaves out the user entry when the process runs as SYSTEM or as the Administrators group.
func identityFileACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	got := sd.String()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	want := []string{";;;SY)", ";;;BA)"}
	if sid != "S-1-5-18" && sid != "S-1-5-32-544" {
		want = append(want, ";;;"+sid+")")
	}
	if !strings.HasPrefix(got, "D:P") || strings.Count(got, "(A;") != len(want) {
		t.Fatalf("access list of %s = %s, want a protected list with %d entries", path, got, len(want))
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("access list of %s = %s, lacks %s", path, got, w)
		}
	}
}
