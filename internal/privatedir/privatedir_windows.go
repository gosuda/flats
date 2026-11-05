package privatedir

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
)

// ErrNotOwnDirectory reports a directory that holds more than Flats data. The
// access list reaches every file below it, so applying it there would take
// access away from other users and from other programs of this user's account.
var ErrNotOwnDirectory = errors.New("the data directory must be a directory of its own")

// ErrForeignOwner reports a directory that another user owns. The owner of a
// directory can change its access list at any time, so no list makes it private.
var ErrForeignOwner = errors.New("the directory belongs to another user")

// markers are names that only Flats uses in a data directory. config.json counts
// only with Flats's schema key: other programs use that name too.
var markers = []string{"flats.db", "flats.lock", "secret.key", "zrok-environment"}

// own reports whether dir is empty or holds Flats data, and is no volume root or
// the user's profile. The default data directory is always Flats's own: the
// service definition stays there, even for a service on another data directory.
func own(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	if filepath.Dir(abs) == abs {
		return false, nil
	}
	if home, err := os.UserHomeDir(); err == nil && strings.EqualFold(filepath.Clean(home), abs) {
		return false, nil
	}
	if root, err := os.UserCacheDir(); err == nil && strings.EqualFold(filepath.Join(filepath.Clean(root), "Flats"), abs) {
		return true, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	for _, name := range markers {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true, nil
		}
	}
	config, err := os.ReadFile(filepath.Join(dir, "config.json"))
	return err == nil && bytes.Contains(config, []byte(`"schema_version"`)), nil
}

// hasAccessLists reports whether the volume of dir stores access lists. FAT and
// exFAT do not.
func hasAccessLists(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	root, err := windows.UTF16PtrFromString(filepath.VolumeName(abs) + `\`)
	if err != nil {
		return false, err
	}
	var flags uint32
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return false, err
	}
	return flags&windows.FILE_PERSISTENT_ACLS != 0, nil
}

// entries splits the access list of an SDDL string into its sorted entries, and
// reports whether it is protected. The system keeps the entries in its own order
// and adds the auto-inherited mark.
func entries(sddl string) ([]string, bool) {
	dacl, ok := strings.CutPrefix(sddl, "D:")
	if !ok {
		return nil, false
	}
	flags, rest, found := strings.Cut(dacl, "(")
	if !found {
		return nil, false
	}
	list := strings.Split(strings.TrimSuffix(rest, ")"), ")(")
	slices.Sort(list)
	return list, strings.Contains(flags, "P")
}

// upToDate reports whether dir has exactly the wanted list. The system writes an
// account that has an alias by that alias, such as the built-in Administrator, so
// the wanted list goes through the system too.
func upToDate(dir, want string) bool {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	parsed, err := windows.SecurityDescriptorFromString(want)
	if err != nil {
		return false
	}
	have, protected := entries(sd.String())
	wanted, _ := entries(parsed.String())
	return protected && slices.Equal(have, wanted)
}

// wanted is the access list for the current user and the system alone.
func wanted() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("current user: %w", err)
	}
	return "D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)", nil
}

// Protect gives dir an access list for the current user and the system alone,
// with no inherited entry, and files below it inherit that list. A directory
// outside the user's profile otherwise inherits access for other local users,
// which would expose what is mode 0600 on Unix. An up to date list is left alone,
// because a change walks the whole tree. A volume without access lists cannot be
// protected, and the server still runs there.
func Protect(dir string) error {
	want, err := wanted()
	if err != nil {
		return err
	}
	if upToDate(dir, want) {
		return nil
	}
	lists, err := hasAccessLists(dir)
	if err != nil {
		return err
	}
	if !lists {
		logNoLists(dir)
		return nil
	}
	ok, err := own(dir)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotOwnDirectory
	}
	return setList(dir, want)
}

// Restrict is Protect for a directory that only Flats uses, such as the one of
// the worker sockets. It needs no sign of Flats there. It refuses a directory
// that another user owns, also one with the right list, because the owner can
// change that list later.
func Restrict(dir string) error {
	lists, err := hasAccessLists(dir)
	if err != nil {
		return err
	}
	if !lists {
		logNoLists(dir)
		return nil
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	// An elevated process creates what the Administrators group owns.
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	if !owner.Equals(user.User.Sid) && !owner.Equals(admins) {
		return ErrForeignOwner
	}
	want, err := wanted()
	if err != nil {
		return err
	}
	if upToDate(dir, want) {
		return nil
	}
	return setList(dir, want)
}

func logNoLists(dir string) {
	log.Printf("the directory %s is on a volume without access lists, so other users of this machine can read it", dir)
}

func setList(dir, want string) error {
	sd, err := windows.SecurityDescriptorFromString(want)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
