//go:build !windows

package api

func filesystemRoots() []folderEntry {
	return []folderEntry{{Name: "/", Path: "/"}}
}
