// Command doccheck audits the repository's Markdown links and anchors.
//
//	go run ./internal/doccheck/cmd/doccheck          # errors only; exit 1 on any
//	go run ./internal/doccheck/cmd/doccheck -audit   # also orphans, colocated notes and hashes
//
// The Makefile wraps both as `make docs-check` and `make docs-audit`.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gosuda/flats/internal/doccheck"
)

func main() {
	audit := flag.Bool("audit", false, "also list unreachable docs, colocated notes and hash literals for review")
	root := flag.String("root", "", "repository root (default: the Git work tree containing the current directory)")
	flag.Parse()
	os.Exit(run(*root, *audit))
}

func run(root string, audit bool) int {
	if root == "" {
		var err error
		if root, err = doccheck.RepoRoot("."); err != nil {
			fmt.Fprintln(os.Stderr, "doccheck:", err)
			return 2
		}
	}
	files, err := doccheck.GitFiles(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doccheck:", err)
		return 2
	}
	r, err := doccheck.Check(root, files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doccheck:", err)
		return 2
	}

	fmt.Printf("Checked %d Markdown files.\n", r.Markdown)
	if len(r.Errors) == 0 {
		fmt.Println("PASS  links, anchors and docs/agent/ links")
	} else {
		fmt.Printf("FAIL  %d link problem(s):\n", len(r.Errors))
		for _, f := range r.Errors {
			fmt.Println("  " + f.String())
		}
	}
	if audit {
		printAudit(r)
	} else if n := len(r.Orphans); n > 0 {
		fmt.Printf("NOTE  %d unreachable doc(s); run `make docs-audit` for the review list\n", n)
	}
	if len(r.Errors) > 0 {
		return 1
	}
	return 0
}

func printAudit(r *doccheck.Report) {
	if len(r.Orphans) == 0 {
		fmt.Println("PASS  every doc is reachable from README.md or code")
	} else {
		fmt.Printf("WARN  %d doc(s) nothing links to (link from an index, or delete once confirmed obsolete):\n", len(r.Orphans))
		for _, o := range r.Orphans {
			fmt.Println("  " + o)
		}
	}
	if len(r.Colocated) > 0 {
		fmt.Printf("INFO  %d colocated package/command/example note(s) outside the docs index:\n", len(r.Colocated))
		for _, c := range r.Colocated {
			suffix := ""
			if c.TestOnly {
				suffix = " (referenced by tests)"
			}
			fmt.Println("  " + c.Path + suffix)
		}
	}
	if len(r.Hashes) > 0 {
		var order []string
		lines := map[string][]string{}
		for _, h := range r.Hashes {
			if _, ok := lines[h.File]; !ok {
				order = append(order, h.File)
			}
			lines[h.File] = append(lines[h.File], fmt.Sprint(h.Line))
		}
		fmt.Printf("INFO  %d file(s) with 40/64-character hex literals (remove stale receipts, keep test vectors and pins):\n", len(order))
		for _, f := range order {
			ls := lines[f]
			more := ""
			if len(ls) > 8 {
				ls, more = ls[:8], ", ..."
			}
			fmt.Printf("  %s: %d line(s) (%s%s)\n", f, len(lines[f]), strings.Join(ls, ", "), more)
		}
	}
}
