//go:build !windows

package core

const (
	reloadAdvice      = "send SIGHUP or restart Flats"
	reloadServeAdvice = "send SIGHUP or restart `flats serve`"
)
