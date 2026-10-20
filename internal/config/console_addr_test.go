package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConsoleAddrMigration(t *testing.T) {
	addresses := []string{
		"", "127.0.0.1:7878", "0.0.0.0:7878", ":7878", "100.64.0.7:8787",
		"[::1]:9000", "[::]:7878", "[fe80::1%lo0]:7878", "localhost:07878",
		"127.0.0.1:0", "127.0.0.1:", "127.0.0.1:65535",
	}
	for version := 1; version <= 4; version++ {
		for _, address := range addresses {
			t.Run(fmt.Sprintf("v%d/%s", version, address), func(t *testing.T) {
				host := map[string]any{"instance_id": testID, "data_dir": "/var/lib/flats"}
				if address != "" {
					host["management_addr"] = address
				}
				raw, err := json.Marshal(map[string]any{"schema_version": version, "host": host})
				if err != nil {
					t.Fatal(err)
				}
				before := bytes.Clone(raw)
				loaded, err := Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(raw, before) {
					t.Fatal("Parse changed input bytes")
				}
				if !loaded.Migrated || loaded.FileVersion != version || loaded.NeedsConfirm || loaded.Doc.SchemaVersion() != 5 {
					t.Fatalf("migration metadata: %+v", loaded)
				}
				cfg, err := loaded.Doc.Effective(nil)
				if err != nil {
					t.Fatal(err)
				}
				want := address
				if want == "" {
					want = "127.0.0.1:7878"
				}
				if cfg.Host.ConsoleAddr != want {
					t.Fatalf("console address = %q, want %q", cfg.Host.ConsoleAddr, want)
				}
				stored, present := loaded.Doc.Stored("host.console_addr")
				if present != (address != "") || present && stored != address {
					t.Fatalf("stored address = %v, %t; input %q", stored, present, address)
				}
				if _, present := loaded.Doc.Stored("host.management_addr"); present || bytes.Contains(loaded.Doc.Encode(), []byte("management_addr")) {
					t.Fatal("migration retained legacy key")
				}
				again, err := Parse(loaded.Doc.Encode())
				if err != nil || again.Migrated || !bytes.Equal(again.Doc.Encode(), loaded.Doc.Encode()) {
					t.Fatalf("migrated config did not round trip: %v", err)
				}
			})
		}
	}
}

func TestConsoleAddrMigrationRejectsConflictsAndInvalidValues(t *testing.T) {
	for version := 1; version <= 5; version++ {
		for _, newAddress := range []string{"127.0.0.1:7878", "0.0.0.0:9000"} {
			t.Run(fmt.Sprintf("conflict/v%d/%s", version, newAddress), func(t *testing.T) {
				raw := fmt.Sprintf(`{"schema_version":%d,"host":{"instance_id":%q,"data_dir":"/var/lib/flats","management_addr":"127.0.0.1:7878","console_addr":%q}}`, version, testID, newAddress)
				if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "host.management_addr") ||
					version < 5 && !strings.Contains(err.Error(), "host.console_addr") {
					t.Fatalf("conflict error = %v", err)
				}
			})
		}
		for _, value := range []string{`7878`, `true`, `[]`, `{}`, `null`, `""`, `"127.0.0.1"`, `"127.0.0.1:65536"`} {
			t.Run(fmt.Sprintf("invalid/v%d/%s", version, value), func(t *testing.T) {
				key := "management_addr"
				if version == 5 {
					key = "console_addr"
				}
				raw := fmt.Sprintf(`{"schema_version":%d,"host":{"instance_id":%q,"data_dir":"/var/lib/flats",%q:%s}}`, version, testID, key, value)
				if _, err := Parse([]byte(raw)); err == nil {
					t.Fatal("accepted invalid address")
				}
			})
		}
	}
}

func TestConsoleAddrCanonicalKey(t *testing.T) {
	for version := 1; version <= 5; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			raw := fmt.Sprintf(`{"schema_version":%d,"host":{"instance_id":%q,"data_dir":"/var/lib/flats","console_addr":"0.0.0.0:9000"}}`, version, testID)
			loaded, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := loaded.Doc.Effective(nil)
			if err != nil || cfg.Host.ConsoleAddr != "0.0.0.0:9000" {
				t.Fatalf("canonical address = %+v, %v", cfg, err)
			}
		})
	}
	if _, err := Parse([]byte(fileWith("host", `"management_addr":"127.0.0.1:7878"`))); errPath(err) != "host.management_addr" {
		t.Fatalf("schema 5 legacy key: %v", err)
	}
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(Keys(), "host.console_addr") || slices.Contains(Keys(), "host.management_addr") {
		t.Fatal("key discovery contains wrong address key")
	}
	if _, ok := Default("host.management_addr"); ok {
		t.Fatal("legacy key has a default")
	}
	if err := doc.Set("host.console_addr", "100.64.0.7:9000"); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{doc.Set("host.management_addr", "127.0.0.1:7878"), doc.Unset("host.management_addr")} {
		if errPath(err) != "host.management_addr" {
			t.Fatalf("legacy key operation: %v", err)
		}
	}
	if _, err := doc.Effective(map[string]string{"host.management_addr": "127.0.0.1:9000"}); errPath(err) != "host.management_addr" {
		t.Fatalf("legacy override: %v", err)
	}
	cfg, err := doc.Effective(map[string]string{"host.console_addr": "[::]:9000"})
	if err != nil || cfg.Host.ConsoleAddr != "[::]:9000" {
		t.Fatalf("canonical override = %+v, %v", cfg, err)
	}
	if _, _, ok := cfg.Lookup("host.management_addr"); ok {
		t.Fatal("legacy lookup succeeded")
	}
	if err := doc.Unset("host.console_addr"); err != nil {
		t.Fatal(err)
	}
	cfg, err = doc.Effective(nil)
	if err != nil || cfg.Host.ConsoleAddr != "127.0.0.1:7878" {
		t.Fatalf("unset default = %+v, %v", cfg, err)
	}
}

func TestConsoleAddrMigrationPersistence(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			extra := ""
			if conflict {
				extra = `,"console_addr":"0.0.0.0:9000"`
			}
			raw := fmt.Sprintf(`{"schema_version":4,"host":{"instance_id":%q,"data_dir":%q,"management_addr":"0.0.0.0:9000"%s}}`, testID, dir, extra)
			hash := writeFile(t, path, raw)
			_, err := Load(path)
			if (err != nil) != conflict {
				t.Fatalf("Load error = %v, conflict %t", err, conflict)
			}
			if readString(t, path) != raw || len(historyFiles(t, dir)) != 0 {
				t.Fatal("Load changed config or history")
			}
			_, err = NewWriter(path).Save(hash, nil)
			if (err != nil) != conflict {
				t.Fatalf("Save error = %v, conflict %t", err, conflict)
			}
			if conflict {
				if readString(t, path) != raw || len(historyFiles(t, dir)) != 0 {
					t.Fatal("failed migration changed config or history")
				}
				return
			}
			history := historyFiles(t, dir)
			if len(history) != 1 || !strings.HasPrefix(history[0], "config.v4.") {
				t.Fatalf("history = %v", history)
			}
			backup, err := os.ReadFile(filepath.Join(dir, historyDir, history[0]))
			if err != nil || string(backup) != raw {
				t.Fatalf("history did not preserve old bytes: %v", err)
			}
			loaded, err := Load(path)
			if err != nil || loaded.FileVersion != 5 || loaded.Migrated {
				t.Fatalf("saved migration = %+v, %v", loaded, err)
			}
		})
	}
}
