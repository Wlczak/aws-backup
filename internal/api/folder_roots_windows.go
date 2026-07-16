//go:build windows

package api

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func filesystemRoots() []folderEntry {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return []folderEntry{}
	}
	roots := make([]folderEntry, 0, 26)
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		path := fmt.Sprintf("%c:\\", 'A'+i)
		roots = append(roots, folderEntry{Name: path, Path: path})
	}
	return roots
}
