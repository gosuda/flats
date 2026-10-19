// Command benchdata writes a reference board data file of the board success
// criteria ("mixed 5k" or "mixed 10k"):
//
//	go run ./internal/board/benchdata/cmd/benchdata -mix 5k -o mixed-5k.zip
//
// Import it into a board with `flats board import <slug> <file> --replace`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gosuda/flats/internal/board/benchdata"
)

func main() {
	mix := flag.String("mix", "5k", "data set: 5k or 10k")
	out := flag.String("o", "", "output file (default mixed-<mix>.zip)")
	seed := flag.Uint64("seed", benchdata.Seed, "generator seed; the reference data uses the default")
	flag.Parse()
	m, err := benchdata.ByName(*mix)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	path := *out
	if path == "" {
		path = "mixed-" + *mix + ".zip"
	}
	b, err := benchdata.WriteFile(path, m, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st, err := os.Stat(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var img int64
	for _, a := range b.Assets {
		img += int64(len(a.Data))
	}
	fmt.Printf("%s: %d elements, %d images (%d bytes), seed %d -> %s (%d bytes)\n",
		m.Name, len(b.Elements), len(b.Assets), img, *seed, path, st.Size())
}
