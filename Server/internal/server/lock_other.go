//go:build !unix

package server

// LockDataDir is a no-op where flock isn't available.
func LockDataDir(dir string) (release func(), err error) { return func() {}, nil }
