package approval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultIDFile keeps the last request id across restarts.
const DefaultIDFile = "/var/lib/latch/last-request-id"

// LoadLastID reads the last request id from path. A missing file means no
// request was made yet.
func LoadLastID(path string) (int64, error) {
	b, err := os.ReadFile(path) //nolint:gosec // fixed path owned by root
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: not a request id", path)
	}
	return n, nil
}

// IDSaver returns a SaveID function that writes each id to path, replacing
// the file in one step so a crash never leaves it half written.
func IDSaver(path string) func(int64) error {
	return func(id int64) error {
		tmp, err := os.CreateTemp(filepath.Dir(path), ".last-request-id-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(strconv.FormatInt(id, 10) + "\n"); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), path)
	}
}
