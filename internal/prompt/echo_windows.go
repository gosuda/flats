package prompt

import "golang.org/x/sys/windows"

// echoOff turns echo off on the console fd with the mode term.ReadPassword
// sets, and returns the function that puts the saved mode back. Without line
// input the console delivers keys one at a time; readHiddenLine edits them.
func echoOff(fd int) (restore func(), err error) {
	h := windows.Handle(fd)
	var saved uint32
	if err := windows.GetConsoleMode(h, &saved); err != nil {
		return nil, err
	}
	mode := saved&^(windows.ENABLE_ECHO_INPUT|windows.ENABLE_LINE_INPUT) | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_PROCESSED_INPUT
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(h, saved) }, nil
}
