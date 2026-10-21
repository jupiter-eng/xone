// Package xutil holds the small general-purpose helpers.
//
// There is one rule for admission: it has to be a pure function or a pure data structure that is **independent
// of the framework** -- usable unchanged in another project. A helper that exists to work around some design
// problem does not belong here; that means the design is what needs fixing.
package xutil

import "os"

// FileExist reports whether the path exists and is a file.
func FileExist(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && !stat.IsDir()
}

// DirExist reports whether the path exists and is a directory.
func DirExist(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.IsDir()
}
