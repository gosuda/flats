//go:build !windows

package site

import "os"

func allowedName(string) bool { return true }

func exactName(*os.File, string) bool { return true }
