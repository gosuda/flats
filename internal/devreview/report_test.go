//go:build unix

package devreview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReport(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, identifies, nil)
	o := f.opts()
	rec, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	f.reviewer(t, "cat >/dev/null", nil)
	o.Copy, o.WritesPrevented = "", "single executor"
	incomplete, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ReadManifest(f.manifest)

	out := filepath.Join(f.evidence, "report.md")
	if _, err := WriteReport(ReportOptions{Root: f.root, ManifestPath: f.manifest, RunsDir: o.RunsDir, Output: out, Stage: "implementation"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	report := string(b)
	for _, want := range []string{
		"## Scope", "## Requirements and base versions", "## Candidate snapshot", "## Material authors",
		"## Review invocations", "## Verdicts and findings", "## Dispositions", "## Verification evidence",
		"## Budget", "## Limitations", "## Next action", "Execution state:", "Candidate verdict:",
		m.Digest, m.Head.Commit, m.Base.MergeBase, "`a.txt`", "`model-a`",
		rec.ID, incomplete.ID, "`s-1`", "materialized-copy", "live-tree",
		filepath.Join(o.RunsDir, rec.ID, "stdout.txt"),
		"Invocation budget: 10, 2 used",
		"write prevention rests on the executor's statement",
		"evidence is incomplete",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	// The report points to the receipt; it never copies or writes a verdict.
	if strings.Contains(report, "VERDICT") {
		t.Error("the report contains reviewer output")
	}

	if _, err := WriteReport(ReportOptions{Root: f.root, ManifestPath: f.manifest, Output: out}); err == nil {
		t.Error("an existing report was overwritten")
	}
	if _, err := WriteReport(ReportOptions{Root: f.root, ManifestPath: f.manifest, Output: filepath.Join(f.root, "report.md")}); !errors.Is(err, ErrInsideRepo) {
		t.Errorf("report inside the repository: err = %v", err)
	}
	bare := filepath.Join(f.evidence, "bare.md")
	if _, err := WriteReport(ReportOptions{Root: f.root, ManifestPath: f.manifest, Output: bare}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(bare)
	if !strings.Contains(string(b), "No invocation was recorded") || !strings.Contains(string(b), "Invocation budget: none recorded") {
		t.Errorf("report without evidence:\n%s", b)
	}
}
