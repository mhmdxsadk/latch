// Package files implements Latch's read-only file tools and the path checks
// that keep them away from secrets.
//
// The whole disk is readable except a block list: built-in rules (see
// DefaultRules), the operator's files.exclude entries, and the folders
// bind-mounted into excluded containers. A blocked path behaves exactly like
// a missing one.
//
// Every path is untrusted. Guard resolves a requested path one component at a
// time, checking each step against the block list before touching it, so a
// symlink cannot lead into a blocked tree and the existence of files inside
// one cannot be probed. The resolved path is then opened without following
// any symlink, and on Linux the opened file's real path is read back from
// /proc/self/fd and checked again.
//
// Guard is the shared gate for every file operation, so a future edit path can
// reuse Check and the same rules.
package files

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/excluded"
)

// Errors returned by Guard and Reader. The mcp package maps them to fixed
// messages; ErrBlocked and ErrNotFound read the same to the agent.
var (
	ErrInvalidPath = errors.New("invalid path")
	ErrBlocked     = errors.New("path is blocked")
	ErrNotFound    = errors.New("path does not exist")
	ErrPermission  = errors.New("permission denied")
	ErrNotFile     = errors.New("not a regular file")
	ErrNotDir      = errors.New("not a directory")
	ErrBinary      = errors.New("binary file")
	ErrBadRequest  = errors.New("invalid request")
	// ErrUnavailable means the excluded containers' folders are not known
	// yet, so no file can be shown safely.
	ErrUnavailable = errors.New("excluded container folders unknown")
)

// maxLinks bounds symlink hops during resolution, like the kernel's limit.
const maxLinks = 40

// ValidatePath reports ErrInvalidPath unless p is a clean absolute path of at
// most config.MaxFilePathLen bytes, valid UTF-8, without control characters
// (so it also has no NUL and is safe as an audit label).
func ValidatePath(p string) error {
	if !config.ValidFilePath(p) {
		return ErrInvalidPath
	}
	return nil
}

// DirPattern blocks the entries directly inside Dir whose name matches
// Pattern (path.Match syntax), and everything below them.
type DirPattern struct {
	Dir     string
	Pattern string
}

// NamedDirPattern blocks the entries directly inside any directory named Dir
// whose name matches Pattern, and everything below them.
type NamedDirPattern struct {
	Dir     string
	Pattern string
}

// Rules is a static block list.
//
// Every rule is closed under descent: if a path is blocked, so is everything
// below it. Resolution relies on this to check each step only once.
type Rules struct {
	// Trees are clean absolute paths; each blocks itself and everything below.
	Trees []string
	// Names are name patterns (path.Match syntax, no slash). A path is
	// blocked if any of its components matches.
	Names []string
	// DirEntries block matching entries of specific directories.
	DirEntries []DirPattern
	// NamedDirEntries block matching entries of directories with a name.
	NamedDirEntries []NamedDirPattern
	// SSH blocks everything inside a directory named .ssh except files named
	// *.pub, authorized_keys, known_hosts and config.
	SSH bool
	// HardLinks are files whose other hard links are refused too. Only these
	// are checked (by device and inode), because finding every link to a file
	// would mean scanning the disk.
	HardLinks []string
}

// BuiltinTrees, BuiltinNames and BuiltinDirEntries are always blocked.
var (
	BuiltinTrees = []string{
		"/proc", "/sys", "/dev",
		"/etc/shadow", "/etc/gshadow", "/etc/shadow-", "/etc/gshadow-",
		"/var/lib/docker", "/var/lib/containerd",
		// Container runtime state: per-container config with env and command.
		"/run/containerd", "/run/docker", "/run/runc",
		"/etc/latch", "/var/lib/latch",
	}
	BuiltinNames = []string{
		"*.key", "*.pem", ".env", ".env.*",
		// Credential files that hold plain passwords or tokens.
		".netrc", ".pgpass", ".git-credentials", ".my.cnf",
	}
	BuiltinDirEntries = []DirPattern{{Dir: "/etc/ssh", Pattern: "*_key"}}
	// BuiltinNamedDirEntries block files by name inside a directory of a
	// given name, wherever it is (registry, cloud and cluster credentials).
	BuiltinNamedDirEntries = []NamedDirPattern{
		{Dir: ".docker", Pattern: "config.json"},
		{Dir: ".aws", Pattern: "credentials"},
		{Dir: ".kube", Pattern: "config"},
	}
	builtinHardLinks = []string{"/etc/shadow", "/etc/gshadow", "/etc/shadow-", "/etc/gshadow-"}
)

// DefaultRules returns the built-in block list plus Latch's own config file,
// the agent accounts' home directories and the operator's files.exclude
// entries (already validated by config). configPath must be absolute.
func DefaultRules(configPath string, agentHomes, exclude []string) Rules {
	r := Rules{
		Trees:           slices.Clone(BuiltinTrees),
		Names:           slices.Clone(BuiltinNames),
		DirEntries:      slices.Clone(BuiltinDirEntries),
		NamedDirEntries: slices.Clone(BuiltinNamedDirEntries),
		SSH:             true,
		HardLinks:       slices.Clone(builtinHardLinks),
	}
	if configPath != "" {
		r.Trees = append(r.Trees, filepath.Clean(configPath))
		r.HardLinks = append(r.HardLinks, filepath.Clean(configPath))
	}
	r.Trees = append(r.Trees, agentHomes...)
	for _, e := range exclude {
		if strings.HasPrefix(e, "/") {
			r.Trees = append(r.Trees, e)
		} else {
			r.Names = append(r.Names, e)
		}
	}
	return r
}

// Folders supplies the host folders mounted into excluded containers.
// *excluded.Tracker implements it.
type Folders interface {
	Get(ctx context.Context) (excluded.Snapshot, error)
}

// Guard decides which paths may be touched.
type Guard struct {
	trees      []string
	names      []string
	dirEntries []DirPattern
	// namedDirEntries are matched on path components, like names.
	namedDirEntries []NamedDirPattern
	ssh             bool
	hardLinks       []string
	folders         Folders
}

// NewGuard builds a Guard. Tree and directory paths that are symlinks (or lie
// under one) are blocked under their resolved path too. folders may be nil
// when there is no Docker, in which case no container folders are blocked.
func NewGuard(r Rules, folders Folders) *Guard {
	g := &Guard{
		trees:           withResolved(r.Trees),
		names:           slices.Clone(r.Names),
		namedDirEntries: slices.Clone(r.NamedDirEntries),
		ssh:             r.SSH,
		hardLinks:       slices.Clone(r.HardLinks),
		folders:         folders,
	}
	for _, d := range r.DirEntries {
		for _, dir := range withResolved([]string{d.Dir}) {
			g.dirEntries = append(g.dirEntries, DirPattern{Dir: dir, Pattern: d.Pattern})
		}
	}
	return g
}

// withResolved returns the clean absolute paths plus, for each that resolves
// to a different real path, that real path.
func withResolved(paths []string) []string {
	var out []string
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		out = append(out, p)
		if real := resolveExisting(p); real != p {
			out = append(out, real)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// resolveExisting resolves the deepest existing ancestor of p and appends the
// rest, so a blocked path that does not exist yet is still blocked under its
// real location (for example /etc/x when /etc is a symlink).
func resolveExisting(p string) string {
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		if dir == "/" {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// Check validates p, resolves it and returns the resolved path, or an error
// if p is invalid, missing or blocked (at any step of resolution). It is the
// entry point for any operation on a path the agent supplied.
func (g *Guard) Check(ctx context.Context, p string) (string, error) {
	if err := ValidatePath(p); err != nil {
		return "", err
	}
	c, err := g.session(ctx)
	if err != nil {
		return "", err
	}
	real, _, err := c.resolve(p)
	return real, err
}

// check is the block list for one tool call: the static rules plus a
// snapshot of the excluded containers' folders.
type check struct {
	g     *Guard
	extra []string

	linksLoaded bool
	links       []fs.FileInfo
}

func (g *Guard) session(ctx context.Context) (*check, error) {
	c := &check{g: g}
	if g.folders != nil {
		snap, err := g.folders.Get(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		c.extra = withResolved(snap.Folders)
	}
	return c, nil
}

func under(p, tree string) bool {
	return tree == "/" || p == tree || strings.HasPrefix(p, tree+"/")
}

func matchAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// sshAllowed are the only names readable directly inside a .ssh directory.
func sshAllowed(name string) bool {
	switch name {
	case "authorized_keys", "known_hosts", "config":
		return true
	}
	return strings.HasSuffix(name, ".pub")
}

// blocked reports whether the clean absolute path p is blocked.
func (c *check) blocked(p string) bool {
	for _, t := range c.g.trees {
		if under(p, t) {
			return true
		}
	}
	for _, t := range c.extra {
		if under(p, t) {
			return true
		}
	}
	if p == "/" {
		return false
	}
	comps := strings.Split(p[1:], "/")
	for i, name := range comps {
		if matchAny(c.g.names, name) {
			return true
		}
		if i+1 < len(comps) {
			for _, d := range c.g.namedDirEntries {
				if name == d.Dir {
					if ok, _ := path.Match(d.Pattern, comps[i+1]); ok {
						return true
					}
				}
			}
		}
		if c.g.ssh && name == ".ssh" && i+1 < len(comps) {
			if i+2 < len(comps) || !sshAllowed(comps[i+1]) {
				return true
			}
		}
	}
	for _, d := range c.g.dirEntries {
		prefix := d.Dir + "/"
		if d.Dir == "/" {
			prefix = "/"
		}
		if rel, ok := strings.CutPrefix(p, prefix); ok {
			first, _, _ := strings.Cut(rel, "/")
			if ok, _ := path.Match(d.Pattern, first); ok {
				return true
			}
		}
	}
	return false
}

// resolve walks p one component at a time, following symlinks itself. Every
// intermediate path is checked against the block list before it is looked at,
// so nothing inside a blocked tree is ever stat'ed: resolution through a
// blocked directory fails with ErrBlocked whether or not the target exists.
// It returns the resolved path (no symlinks in any component) and its Lstat.
func (c *check) resolve(p string) (string, fs.FileInfo, error) {
	if c.blocked(p) {
		return "", nil, ErrBlocked
	}
	cur := "/"
	rest := p
	var info fs.FileInfo
	links := 0
	for {
		rest = strings.TrimLeft(rest, "/")
		if rest == "" {
			break
		}
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		switch name {
		case ".":
			continue
		case "..":
			// cur has no symlinks, so its parent is the real parent. Rules are
			// closed under descent, so an ancestor of an allowed path is
			// allowed.
			cur, info = filepath.Dir(cur), nil
			continue
		}
		next := filepath.Join(cur, name)
		if c.blocked(next) {
			return "", nil, ErrBlocked
		}
		fi, err := os.Lstat(next)
		if err != nil {
			return "", nil, mapErr(err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			links++
			if links > maxLinks {
				return "", nil, ErrNotFound
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", nil, mapErr(err)
			}
			if target == "" {
				return "", nil, ErrNotFound
			}
			if strings.HasPrefix(target, "/") {
				cur = "/"
			}
			rest = target + "/" + rest
			info = nil
			continue
		}
		if strings.Trim(rest, "/") != "" && !fi.IsDir() {
			return "", nil, ErrNotFound
		}
		cur, info = next, fi
	}
	if c.blocked(cur) {
		return "", nil, ErrBlocked
	}
	if info == nil {
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", nil, mapErr(err)
		}
		info = fi
	}
	return cur, info, nil
}

// mapErr turns a filesystem error into one of the package errors. Unknown
// errors are returned wrapped; callers treat them as integration errors.
func mapErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR),
		errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENAMETOOLONG):
		return ErrNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return ErrPermission
	}
	return err
}

type kind int

const (
	kindFile kind = iota
	kindDir
)

func wantType(m fs.FileMode, k kind) error {
	switch {
	case k == kindFile && m.IsRegular():
		return nil
	case k == kindDir && m.IsDir():
		return nil
	case k == kindFile:
		return ErrNotFile
	default:
		return ErrNotDir
	}
}

// open opens the resolved path real for reading. Its type is checked with
// Lstat before opening (so devices and FIFOs are never opened), then the
// file is opened without following any symlink, its real path is verified
// where the platform allows, and its identity and type are re-checked with
// fstat. A regular file that is a hard link to a protected file is refused.
func (c *check) open(real string, k kind) (*os.File, fs.FileInfo, error) {
	if c.blocked(real) {
		return nil, nil, ErrBlocked
	}
	pre, err := os.Lstat(real)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	if err := wantType(pre.Mode(), k); err != nil {
		return nil, nil, err
	}
	fd, err := openNoFollow(real, k == kindDir)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	f := os.NewFile(uintptr(fd), real) //nolint:gosec // fd is a valid, non-negative descriptor from open
	fail := func(err error) (*os.File, fs.FileInfo, error) {
		f.Close()
		return nil, nil, err
	}
	got, err := fdPath(fd)
	switch {
	case errors.Is(err, errNoFDPath):
		// The platform cannot report it; openNoFollow already refused every
		// symlink on the way.
	case err != nil:
		return fail(ErrNotFound)
	case got != real || c.blocked(got):
		return fail(ErrNotFound)
	}
	if err := checkFS(fd); err != nil {
		return fail(ErrBlocked)
	}
	st, err := f.Stat()
	if err != nil {
		return fail(mapErr(err))
	}
	if !os.SameFile(pre, st) {
		return fail(ErrNotFound)
	}
	if err := wantType(st.Mode(), k); err != nil {
		return fail(err)
	}
	if k == kindFile && nlink(st) > 1 && c.hardLinked(st) {
		return fail(ErrBlocked)
	}
	return f, st, nil
}

// hardLinked reports whether st is the same file as one of the protected
// HardLinks files.
func (c *check) hardLinked(st fs.FileInfo) bool {
	if !c.linksLoaded {
		c.linksLoaded = true
		for _, p := range c.g.hardLinks {
			if fi, err := os.Stat(p); err == nil {
				c.links = append(c.links, fi)
			}
		}
	}
	for _, fi := range c.links {
		if os.SameFile(fi, st) {
			return true
		}
	}
	return false
}

// nlink returns the hard link count of a file, or 1 if unknown.
func nlink(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return toU64(st.Nlink)
	}
	return 1
}

// toU64 widens a platform-sized unsigned integer.
func toU64[T ~uint16 | ~uint32 | ~uint64](v T) uint64 { return uint64(v) }
