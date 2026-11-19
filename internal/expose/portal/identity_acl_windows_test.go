package portal

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileAllAccess is FILE_ALL_ACCESS, the right "FA" in the list that the Portal SDK writes.
const fileAllAccess = 0x1F01FF

// identityFileACL checks the access list that the Portal SDK gives an identity file on Windows (Windows support spec,
// rule 18), for the user of the process token.
func identityFileACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if problem := identityACLProblem(sd, user.User.Sid); problem != "" {
		t.Fatalf("access list of %s = %s: %s", path, sd, problem)
	}
}

// identityACLProblem returns what is wrong with sd as the access list of an identity file of user, or "": the list
// must be protected and hold one allow entry with full access for each of SYSTEM, the Administrators group and user,
// none of them inherited, and no other entry. The SDK leaves out the user entry when user is SYSTEM or the
// Administrators group. The entries are compared as SIDs, not as SDDL text: the system writes an account that has an
// alias by that alias, such as LA for the built-in Administrator.
func identityACLProblem(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) string {
	control, _, err := sd.Control()
	if err != nil {
		return err.Error()
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return "the list is not protected"
	}
	want := map[string]bool{"S-1-5-18": false, "S-1-5-32-544": false}
	if sid := user.String(); sid != "S-1-5-18" && sid != "S-1-5-32-544" {
		want[sid] = false
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err.Error()
	}
	if acl == nil || int(acl.AceCount) != len(want) {
		return "the list does not have one entry for each of SYSTEM, Administrators and the user"
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err.Error()
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		seen, ok := want[sid]
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 ||
			ace.Mask != fileAllAccess || !ok || seen {
			return "an entry is not a full-access allow entry of its own for SYSTEM, Administrators or the user"
		}
		want[sid] = true
	}
	return ""
}

// The check of the list accepts the list of the SDK for each kind of user, also an account that the system writes by
// an alias, and refuses a list that lacks an entry, has another account or a deny entry, inherits, or gives less
// access. The cases do not depend on the user of the test process.
func TestIdentityACLCheck(t *testing.T) {
	sid := func(s string) *windows.SID {
		t.Helper()
		v, err := windows.StringToSid(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const me = "S-1-5-21-1000-2000-3000-1001" // an ordinary account of some domain
	user := sid(me)
	admin := sid("LA") // the built-in Administrator of this machine
	cases := []struct {
		name string
		sddl string
		user *windows.SID
		ok   bool
	}{
		{"the SDK list for the user", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + me + ")", user, true},
		{"another order", "D:P(A;;FA;;;" + me + ")(A;;FA;;;SY)(A;;FA;;;BA)", user, true},
		{"the built-in Administrator", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + admin.String() + ")", admin, true},
		{"the user SYSTEM", "D:P(A;;FA;;;SY)(A;;FA;;;BA)", sid("S-1-5-18"), true},
		{"the user Administrators", "D:P(A;;FA;;;SY)(A;;FA;;;BA)", sid("S-1-5-32-544"), true},
		{"no user entry", "D:P(A;;FA;;;SY)(A;;FA;;;BA)", user, false},
		{"another account", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)", user, false},
		{"one more account", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + me + ")(A;;FA;;;WD)", user, false},
		{"a deny entry", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(D;;FA;;;" + me + ")", user, false},
		{"one account twice", "D:P(A;;FA;;;SY)(A;;FA;;;SY)(A;;FA;;;" + me + ")", user, false},
		{"less access", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + me + ")", user, false},
		{"an inherited entry", "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;ID;FA;;;" + me + ")", user, false},
		{"not protected", "D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + me + ")", user, false},
	}
	for _, c := range cases {
		sd, err := windows.SecurityDescriptorFromString(c.sddl)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if problem := identityACLProblem(sd, c.user); (problem == "") != c.ok {
			t.Errorf("%s: %s gives %q, want accepted %v", c.name, sd, problem, c.ok)
		}
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + admin.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the system writes the built-in Administrator %s as %s", admin, sd)
}
