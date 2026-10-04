package core_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// Without a source the settings start at config.json's frozen defaults and
// never touch the database settings table.
func TestSettingsDefaultsComeFromConfig(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.st.SetSetting(ctx, core.SetKeepVersions, "99"); err != nil {
		t.Fatal(err)
	}
	all, err := e.svc.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range all {
		key, ok := core.SettingConfigKey(k)
		if !ok {
			t.Fatalf("setting %s has no config key", k)
		}
		def, _ := config.Default(key)
		if want := core.Defaults[k]; v != want || want != formatDefault(def) {
			t.Errorf("%s = %q, default %q, config default %v", k, v, want, def)
		}
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetKeepVersions: "3"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.st.GetSetting(ctx, core.SetKeepVersions, ""); v != "99" {
		t.Fatalf("settings table written: %q", v)
	}
	// config.json's ranges and rules apply to console values too.
	for k, v := range map[string]string{core.SetKeepVersions: "100001", core.SetPortalDiscover: "false"} {
		if _, err := e.svc.UpdateSettings(ctx, map[string]string{k: v}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
}

func formatDefault(v any) string {
	switch v := v.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	case []string:
		if len(v) == 0 {
			return ""
		}
	}
	return "?"
}

// A source saves before it applies: a failed save changes nothing, and a
// conflict is reported as ErrConflict.
func TestSettingsSourceSavesBeforeApplying(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	doc, err := config.New(config.NewInstanceID(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var saved *config.Document
	fail := error(nil)
	src, err := core.NewSettingsSource(doc, func(_ context.Context, apply func(*config.Document) error) error {
		if fail != nil {
			return fail
		}
		saved = doc.Clone()
		return apply(saved)
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Settings: src, Logf: t.Logf, Now: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetUploadMaxBytes: "4096", core.SetPortalRelays: "https://rly.best"}); err != nil {
		t.Fatal(err)
	}
	if svc.UploadLimit() != 4096 || saved == nil {
		t.Fatal("save or apply missing")
	}
	if c, _ := saved.Effective(nil); c.System.UploadMaxBytes != 4096 || len(c.Portal.Relays) != 1 {
		t.Fatalf("saved = %+v %+v", c.System, c.Portal)
	}
	fail = &config.ConflictError{}
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetUploadMaxBytes: "8192"}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	fail = errors.New("disk full")
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetUploadMaxBytes: "8192"}); err == nil || errors.Is(err, core.ErrInvalid) {
		t.Fatalf("write failure: %v", err)
	}
	if svc.UploadLimit() != 4096 {
		t.Fatal("a failed save was applied")
	}
}

func TestMissingSecretKeyWithSecretsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSecret(ctx, "app", "TOKEN", "v", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := core.CheckSecretKey(ctx, e.dataDir, e.st); err != nil {
		t.Fatal(err)
	}
	if err := core.CheckSecretKey(ctx, t.TempDir(), e.st); !errors.Is(err, core.ErrSecretKeyMissing) {
		t.Fatalf("missing key over secrets: %v", err)
	}
	if _, err := core.New(ctx, core.Config{DataDir: t.TempDir(), Store: e.st, Logf: t.Logf}); !errors.Is(err, core.ErrSecretKeyMissing) {
		t.Fatalf("core.New created a key over secrets: %v", err)
	}
}
