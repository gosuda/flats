package privatedir

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func dacl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func TestProtectLeavesOnlyTheUserAndSystem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flats.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Protect(dir); err != nil {
		t.Fatal(err)
	}
	got := dacl(t, dir)
	if !strings.HasPrefix(got, "D:P") {
		t.Fatalf("the list still inherits: %s", got)
	}
	if strings.Count(got, "(A;") != 2 {
		t.Fatalf("want two entries: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"flats.db", "new"} {
		if got := dacl(t, filepath.Join(dir, name)); strings.Count(got, "(A;") != 2 || strings.Contains(got, ";BU)") || strings.Contains(got, ";WD)") {
			t.Fatalf("%s: %s", name, got)
		}
	}
}

// The check must see the list the system stored, in the system's order, or every
// start would walk the whole tree.
func TestProtectRecognizesItsOwnList(t *testing.T) {
	dir := t.TempDir()
	if err := Protect(dir); err != nil {
		t.Fatal(err)
	}
	want, err := wanted()
	if err != nil {
		t.Fatal(err)
	}
	if !upToDate(dir, want) {
		t.Fatalf("%s does not match %s", dacl(t, dir), want)
	}
	// A list that is up to date is left alone, also where it would be refused now.
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Protect(dir); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestEntriesIgnoreTheOrderOfTheSystem(t *testing.T) {
	a, protectedA := entries("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;S-1-5-21-1)")
	b, protectedB := entries("D:P(A;OICI;FA;;;S-1-5-21-1)(A;OICI;FA;;;SY)")
	if !protectedA || !protectedB || !slices.Equal(a, b) {
		t.Fatalf("%v %v %v %v", a, protectedA, b, protectedB)
	}
	if _, protected := entries("D:AI(A;OICI;FA;;;SY)"); protected {
		t.Fatal("an inheriting list reads as protected")
	}
}

func TestProtectRefusesADirectoryWithOtherContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thesis.docx"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := dacl(t, dir)
	if err := Protect(dir); !errors.Is(err, ErrNotOwnDirectory) {
		t.Fatalf("got %v", err)
	}
	if after := dacl(t, dir); after != before {
		t.Fatalf("the list changed: %s", after)
	}
}

func TestOwnRefusesRootsAndAcceptsFlatsData(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{home, filepath.VolumeName(home) + `\`} {
		if ok, err := own(dir); err != nil || ok {
			t.Errorf("%s: %v, %v", dir, ok, err)
		}
	}
	empty := t.TempDir()
	if ok, err := own(empty); err != nil || !ok {
		t.Errorf("an empty directory: %v, %v", ok, err)
	}
	for _, marker := range markers {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, err := own(dir); err != nil || !ok {
			t.Errorf("%s: %v, %v", marker, ok, err)
		}
	}
}

func TestOwnReadsConfigJSONAndGenericNames(t *testing.T) {
	for name, body := range map[string]string{
		"config.json": "{\"name\":\"my project\"}",
		"logs":        "",
		"cache":       "",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, err := own(dir); err != nil || ok {
			t.Errorf("%s is accepted: %v, %v", name, ok, err)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{\"schema_version\": 5}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := own(dir); err != nil || !ok {
		t.Errorf("a Flats config.json: %v, %v", ok, err)
	}
}

func TestProtectAcceptsTheDefaultFolderWithOnlyTheServiceDefinition(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LocalAppData", root)
	for name, want := range map[string]error{"Flats": nil, "Other": ErrNotOwnDirectory} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "service.xml"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Protect(dir); !errors.Is(err, want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRestrictLeavesOnlyTheUserAndSystemWithoutASignOfFlats(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "w1.sock"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restrict(dir); err != nil {
		t.Fatal(err)
	}
	want, err := wanted()
	if err != nil {
		t.Fatal(err)
	}
	if !upToDate(dir, want) {
		t.Fatalf("list: %s", dacl(t, dir))
	}
}

// The system stores an account with an alias by that alias. The built-in
// Administrator is LA, which only that account can show, and SYSTEM is SY.
func TestUpToDateReadsAnAccountByItsAlias(t *testing.T) {
	dir := t.TempDir()
	want, err := wanted()
	if err != nil {
		t.Fatal(err)
	}
	full := strings.Replace(want, ";SY)", ";S-1-5-18)", 1)
	if full == want {
		t.Fatalf("no SYSTEM entry in %s", want)
	}
	if err := setList(dir, full); err != nil {
		t.Fatal(err)
	}
	if !upToDate(dir, full) {
		t.Fatalf("%s does not match %s", dacl(t, dir), full)
	}
}

// The owner of a directory can change its list, so another user's directory is
// refused and left as it is.
func TestRestrictRefusesADirectoryOfAnotherOwner(t *testing.T) {
	dir := filepath.Join(os.Getenv("SystemRoot"), "System32")
	before := dacl(t, dir)
	if err := Restrict(dir); !errors.Is(err, ErrForeignOwner) {
		t.Fatal(err)
	}
	if after := dacl(t, dir); after != before {
		t.Fatalf("the list changed: %s", after)
	}
}

func TestThisVolumeHasAccessLists(t *testing.T) {
	ok, err := hasAccessLists(t.TempDir())
	if err != nil || !ok {
		t.Fatalf("%v, %v", ok, err)
	}
}
