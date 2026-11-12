// Package durable holds the file operations whose failure would stop the host:
// a directory change that must survive a power loss, and a rename or removal
// that another process may hold up for a moment on Windows.
package durable
