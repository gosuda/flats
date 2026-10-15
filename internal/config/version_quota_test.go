package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestVersionQuotaMigration(t *testing.T) {
	for _, schema := range []int{1, 2, 3} {
		for _, old := range []string{"", "0", "3", "40"} {
			t.Run(fmt.Sprintf("v%d/%s", schema, old), func(t *testing.T) {
				sys := ""
				if old != "" {
					sys = `,"system":{"keep_versions":` + old + `}`
				}
				raw := fmt.Sprintf(`{"schema_version":%d,"host":{"instance_id":"%s","data_dir":"/var/lib/flats"}%s}`, schema, testID, sys)
				loaded, err := Parse([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := loaded.Doc.Effective(nil)
				want := "20"
				if old != "" {
					want = old
				}
				if err != nil || fmt.Sprint(cfg.System.VersionQuota) != want {
					t.Fatalf("quota: %+v %v want %s", cfg.System, err, want)
				}
				_, present := loaded.Doc.Stored("system.version_quota")
				if present != (old != "") {
					t.Fatalf("explicitness lost: %s", loaded.Doc.Encode())
				}
				if strings.Contains(string(loaded.Doc.Encode()), "keep_versions") {
					t.Fatal("legacy key persisted")
				}
			})
		}
	}
}
