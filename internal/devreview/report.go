package devreview

import (
	"bytes"
	_ "embed"
	"fmt"
	"path/filepath"
	"slices"
	"text/template"
)

//go:embed report.md.tmpl
var reportTemplate string

// ReportOptions describe a task report skeleton.
type ReportOptions struct {
	Root         string   // repository work tree
	ManifestPath string   // snapshot manifest
	RunsDir      string   // evidence directory; may be empty or missing
	Output       string   // report file, created outside the repository
	Stage        string   // optional stage label
	Title        string   // optional title
	Authors      []string // optional material author model IDs
}

type reportData struct {
	ReportOptions
	M           *Manifest
	Records     []*Record
	Budget      *Budget
	Used        int
	Changed     int
	Limitations []string
}

// WriteReport writes a task report skeleton filled from the manifest and
// the invocation records. It never overwrites an existing file and leaves
// verdicts, dispositions and other judgments for the executor to fill in.
func WriteReport(o ReportOptions) (string, error) {
	dest, err := OutsideRepo(o.Root, o.Output)
	if err != nil {
		return "", err
	}
	o.ManifestPath, err = filepath.Abs(o.ManifestPath)
	if err != nil {
		return "", err
	}
	m, err := ReadManifest(o.ManifestPath)
	if err != nil {
		return "", err
	}
	d := reportData{ReportOptions: o, M: m}
	if o.RunsDir != "" {
		if d.RunsDir, err = filepath.Abs(o.RunsDir); err != nil {
			return "", err
		}
		if d.Records, err = LoadRecords(d.RunsDir); err != nil {
			return "", err
		}
		if d.Budget, err = ReadBudget(d.RunsDir); err != nil {
			return "", err
		}
	}
	d.Used = len(d.Records)
	for _, e := range m.Files {
		if e.Changed() {
			d.Changed++
		}
	}
	d.Authors = cleanList(o.Authors)
	if len(d.Authors) == 0 {
		for _, r := range d.Records {
			for _, a := range r.Authors {
				if !slices.Contains(d.Authors, a) {
					d.Authors = append(d.Authors, a)
				}
			}
		}
	}
	d.Limitations = limitations(d.Records, m)
	t, err := template.New("report").Funcs(template.FuncMap{"deref": func(p *int) int { return *p }}).Parse(reportTemplate)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return dest, writeNew(dest, b.Bytes())
}

// limitations lists what the recorded evidence does not establish.
func limitations(records []*Record, m *Manifest) []string {
	var out []string
	for _, r := range records {
		switch {
		case r.Snapshot != "" && r.Snapshot != m.Digest:
			out = append(out, fmt.Sprintf("Invocation `%s` reviewed snapshot `%s`, not this one.", r.ID, r.Snapshot))
		}
		if r.Surface.Kind == SurfaceLive {
			out = append(out, fmt.Sprintf("Invocation `%s` reviewed the live working tree; write prevention rests on the executor's statement: %q.", r.ID, r.Surface.WritesPrevented))
		}
		if r.Outcome != OutcomeComplete {
			out = append(out, fmt.Sprintf("Invocation `%s` evidence is %s (status %s%s).", r.ID, orUnknown(r.Outcome), r.Status, missing(r)))
		}
		if r.Abandoned != nil {
			out = append(out, fmt.Sprintf("Invocation `%s` was settled by the executor, not by the helper: %q.", r.ID, r.Abandoned.Reason))
		}
		if len(r.SessionReused) > 0 {
			out = append(out, fmt.Sprintf("Invocation `%s` continued the session of %v; only a re-review may reuse a session.", r.ID, r.SessionReused))
		}
	}
	return out
}

func missing(r *Record) string {
	var s string
	if r.Identity != nil && r.Identity.Status != "known" {
		s += ", reviewer identity " + r.Identity.Status
	}
	if r.Eligibility != "" && r.Eligibility != "eligible" {
		s += ", eligibility " + r.Eligibility
	}
	if r.After != nil && !r.After.OK {
		s += ", surface after review: " + r.After.Detail
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
