//go:build !linux

package runtime

// privateMemory is not measured outside Linux: there is no CGO-free
// equivalent of RssAnon, and a partial figure would mislead.
func privateMemory() (int64, bool) { return 0, false }
