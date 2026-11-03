package files

import "golang.org/x/sys/unix"

// renameNoReplace renames old to new and fails with EEXIST if new exists.
func renameNoReplace(oldDir int, old string, newDir int, new string) error {
	return unix.Renameat2(oldDir, old, newDir, new, unix.RENAME_NOREPLACE)
}
