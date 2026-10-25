package files

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
)

// AgentHomes returns the home directories of the accounts in group (the
// socket group, whose members are the agents): users listed as members in
// /etc/group plus users whose primary group it is in /etc/passwd. Blocking
// them keeps the agent's own credentials (API keys, auth files, session
// history) away from it.
//
// os/user cannot list group members, so both files are read directly. Users
// that exist only in a network directory (LDAP, SSSD) and have the group as
// their primary group are not found. Home directories that are "/" or not
// clean absolute paths are skipped, since blocking them would block
// everything or nothing.
//
// The returned error describes what could not be resolved; the homes that
// were found are returned with it, and the caller decides whether to carry
// on.
func AgentHomes(group string) ([]string, error) {
	return agentHomes(group, "/etc/group", "/etc/passwd", func(name string) (string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return u.HomeDir, nil
	})
}

func agentHomes(group, groupFile, passwdFile string, lookupHome func(string) (string, error)) ([]string, error) {
	var errs []error
	var homes []string
	add := func(user, home string) {
		if home == "/" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
			errs = append(errs, fmt.Errorf("home directory of %q is not usable: %q", user, home))
			return
		}
		homes = append(homes, home)
	}

	gid := ""
	var members []string
	err := scanColonFile(groupFile, func(f []string) {
		if len(f) >= 4 && f[0] == group {
			gid = f[2]
			for _, m := range strings.Split(f[3], ",") {
				if m = strings.TrimSpace(m); m != "" {
					members = append(members, m)
				}
			}
		}
	})
	if err != nil {
		errs = append(errs, err)
	}
	if gid == "" {
		if g, err := user.LookupGroup(group); err == nil {
			gid = g.Gid
		} else {
			errs = append(errs, fmt.Errorf("group %q not found", group))
		}
	}

	seen := map[string]bool{}
	if gid != "" {
		err := scanColonFile(passwdFile, func(f []string) {
			if len(f) >= 6 && f[3] == gid {
				seen[f[0]] = true
				add(f[0], f[5])
			}
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, m := range members {
		if seen[m] {
			continue
		}
		seen[m] = true
		home, err := lookupHome(m)
		if err != nil {
			errs = append(errs, fmt.Errorf("look up home directory of %q: %w", m, err))
			continue
		}
		add(m, home)
	}
	slices.Sort(homes)
	homes = slices.Compact(homes)
	if len(errs) > 0 {
		return homes, fmt.Errorf("agent home directories: %w", errors.Join(errs...))
	}
	return homes, nil
}

// scanColonFile calls fn with the colon-separated fields of each line of an
// /etc/passwd or /etc/group style file.
func scanColonFile(path string, fn func([]string)) error {
	f, err := os.Open(path) //nolint:gosec // fixed system file
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, 16<<20))
	s.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for s.Scan() {
		line := s.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fn(strings.Split(line, ":"))
	}
	return s.Err()
}
