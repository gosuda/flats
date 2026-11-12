//go:build !windows

package winservice

import "errors"

var errWindowsOnly = errors.New("winservice: Windows only")

func killProcess(int) error { return errWindowsOnly }

func currentUser() (id, name string, err error) { return "", "", errWindowsOnly }
