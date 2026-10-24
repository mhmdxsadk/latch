package files

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mhmdxsadk/latch/internal/excluded"
)

type fakeFolders struct {
	folders []string
	err     error
}

func (f *fakeFolders) Get(context.Context) (excluded.Snapshot, error) {
	return excluded.Snapshot{Folders: f.folders}, f.err
}

// fixture is a temporary tree with a Guard whose block list includes the
// built-in rules plus entries pointing into the tree.
type fixture struct {
	t       *testing.T
	root    string // as created; on macOS it lies under the /var symlink
	cfg     string // Latch's "config file", blocked
	blocked string // a blocked directory
	homes   string // an agent home, blocked
	folders *fakeFolders
	guard   *Guard
	reader  *Reader
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, folders: &fakeFolders{}}
	f.cfg = f.write("etc-latch/config.yaml", "server: {}\n")
	f.blocked = f.mkdir("private")
	f.write("private/secret.txt", "s3cret\n")
	f.mkdir("private/sub")
	f.homes = f.mkdir("home/agent")
	f.write("home/agent/.codex/auth.json", `{"token": "x"}`)
	f.write("public/hello.txt", "hello\nworld\n")
	f.mkdir("container-data")
	f.write("container-data/db.conf", "data\n")
	f.folders.folders = []string{filepath.Join(root, "container-data")}

	rules := DefaultRules(f.cfg, []string{f.homes}, []string{f.blocked, "*.secret", "credentials.json"})
	f.guard = NewGuard(rules, f.folders)
	f.reader = NewReader(f.guard, DefaultLimits())
	return f
}

func (f *fixture) path(rel string) string { return filepath.Join(f.root, rel) }

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := f.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) mkdir(rel string) string {
	f.t.Helper()
	p := f.path(rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) symlink(target, rel string) string {
	f.t.Helper()
	p := f.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) check(p string) (string, error) {
	return f.guard.Check(context.Background(), p)
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{
		"", "etc/shadow", "./etc", "/etc/../etc/shadow", "/etc//shadow", "/etc/", "/etc/.", "/etc/./shadow",
		"/etc/sh\x00adow", "/etc/a\nb", "/etc/a\rb", "/etc/\x1b[31m", "/etc/\xff",
		"/" + strings.Repeat("a", 4096),
	} {
		if err := ValidatePath(p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ValidatePath(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"/", "/etc/hosts", "/srv/a b/ç.txt", "/" + strings.Repeat("a", 4095)} {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v", p, err)
		}
	}
}

func TestBuiltinRules(t *testing.T) {
	g := NewGuard(DefaultRules("/opt/latch/latch.yaml", []string{"/home/agent"}, []string{"/srv/backups", "*.secret", "credentials.json"}), nil)
	c := &check{g: g}
	blocked := []string{
		"/proc", "/proc/self/root/etc/passwd", "/proc/1/environ", "/sys", "/sys/kernel", "/dev", "/dev/null", "/dev/sda",
		"/etc/shadow", "/etc/gshadow", "/etc/shadow-", "/etc/gshadow-",
		"/etc/ssh/ssh_host_ed25519_key", "/etc/ssh/ssh_host_rsa_key", "/etc/ssh/ssh_host_rsa_key/x",
		"/root/.ssh/id_ed25519", "/home/bob/.ssh/id_rsa", "/home/bob/.ssh/authorized_keys2", "/home/bob/.ssh/sub/config",
		"/home/bob/.ssh/known_hosts.old", "/home/bob/.ssh/config/x",
		"/srv/tls/site.pem", "/srv/tls/site.key", "/srv/tls/site.key/inner", "/srv/app/.env", "/srv/app/.env.production",
		"/srv/app/.env/inner",
		"/var/lib/docker", "/var/lib/docker/volumes/x/_data/f", "/etc/latch", "/etc/latch/config.yaml",
		"/var/lib/latch", "/var/lib/latch/backups/1", "/opt/latch/latch.yaml",
		"/home/agent", "/home/agent/.codex/auth.json",
		"/srv/backups", "/srv/backups/db.sql", "/srv/app/x.secret", "/srv/app/credentials.json", "/srv/x.secret/y",
	}
	allowed := []string{
		"/", "/proc2", "/procfoo/x", "/system", "/devices", "/var/lib/dockerfoo", "/var/lib/docker-compose",
		"/var/lib", "/var/lib/latchkey", "/etc/latchd", "/etc/shadowfoo", "/etc/passwd", "/etc/shadow.d",
		"/etc/ssh", "/etc/ssh/sshd_config", "/etc/ssh/ssh_host_ed25519_key.pub", "/etc/ssh/keys/foo_key.txt",
		"/home/bob/.ssh", "/home/bob/.ssh/id_ed25519.pub", "/home/bob/.ssh/authorized_keys", "/home/bob/.ssh/known_hosts",
		"/home/bob/.ssh/config", "/srv/app/.envrc", "/srv/app/env", "/srv/app/site.pem.txt", "/srv/app/keys",
		"/home/agentfoo", "/home/agent2/x", "/opt/latch", "/opt/latch/latch.yaml.bak", "/srv/backups2",
		"/srv/app/credentials.json.example",
	}
	for _, p := range blocked {
		if !c.blocked(p) {
			t.Errorf("%s should be blocked", p)
		}
	}
	for _, p := range allowed {
		if c.blocked(p) {
			t.Errorf("%s should be allowed", p)
		}
	}
}

// Every rule must be closed under descent: resolution checks each step once.
func TestRulesAreClosedUnderDescent(t *testing.T) {
	g := NewGuard(DefaultRules("/opt/latch/latch.yaml", []string{"/home/agent"}, []string{"/srv/backups", "*.secret"}), nil)
	c := &check{g: g}
	for _, p := range []string{"/proc", "/etc/shadow", "/etc/ssh/a_key", "/home/b/.ssh/id_rsa", "/home/b/.ssh/config/x", "/a/b.pem", "/a/.env", "/home/agent", "/srv/backups", "/a/b.secret"} {
		for _, child := range []string{"/x", "/x/y", "/.ssh", "/config"} {
			if !c.blocked(p + child) {
				t.Errorf("%s is blocked but %s is not", p, p+child)
			}
		}
	}
}

func TestBlockedPathsInTree(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		f.cfg, f.blocked, f.path("private/secret.txt"), f.path("private/missing.txt"),
		f.homes, f.path("home/agent/.codex/auth.json"),
		f.path("container-data"), f.path("container-data/db.conf"),
		f.write("public/app/.env.production", "A=1\n"), f.write("public/tls/site.pem", "x\n"),
		f.write("public/home/.ssh/id_ed25519", "k\n"), f.write("public/x.secret", "x\n"),
		"/etc/shadow", "/proc/self/root/etc/passwd", "/dev/null", "/var/lib/docker/containers",
	} {
		if _, err := f.check(p); !errors.Is(err, ErrBlocked) {
			t.Errorf("Check(%s) = %v, want ErrBlocked", p, err)
		}
	}
	pub := f.write("public/home/.ssh/id_ed25519.pub", "ssh-ed25519 AAAA\n")
	for _, p := range []string{pub, f.path("public/hello.txt"), f.path("public")} {
		if _, err := f.check(p); err != nil {
			t.Errorf("Check(%s) = %v", p, err)
		}
	}
}

func TestCheckResolvesSymlinks(t *testing.T) {
	f := newFixture(t)
	link := f.symlink(f.path("public/hello.txt"), "public/link.txt")
	got, err := f.check(link)
	want, _ := filepath.EvalSymlinks(f.path("public/hello.txt"))
	if err != nil || got != want {
		t.Errorf("Check = %q, %v; want %q", got, err, want)
	}
	rel := f.symlink("../public/hello.txt", "other/rel.txt")
	if got, err := f.check(rel); err != nil || got != want {
		t.Errorf("relative link: %q, %v", got, err)
	}
}

func TestSymlinksIntoBlockedTrees(t *testing.T) {
	f := newFixture(t)
	cases := map[string]string{
		"to blocked file":                  f.symlink(f.path("private/secret.txt"), "public/l1"),
		"to config":                        f.symlink(f.cfg, "public/l2"),
		"to shadow":                        f.symlink("/etc/shadow", "public/l3"),
		"to proc root":                     f.symlink("/proc/self/root", "public/l4") + "/etc/passwd",
		"to dev":                           f.symlink("/dev/null", "public/l5"),
		"parent dir into blocked tree":     f.symlink(f.blocked, "public/l6") + "/secret.txt",
		"parent dir, missing target":       f.path("public/l6/nope.txt"),
		"parent dir into home":             f.symlink(f.homes, "public/l7") + "/.codex/auth.json",
		"relative into blocked":            f.symlink("../private/secret.txt", "public/l8"),
		"into container folder":            f.symlink(f.path("container-data"), "public/l9") + "/db.conf",
		"to a .env name":                   f.symlink(f.write("public/real/.env", "A=1\n"), "public/l10"),
		"chain ending blocked":             f.symlink(f.path("public/l1"), "public/l11"),
		"dotdot through blocked, exists":   f.symlink(f.blocked+"/sub/../../public/hello.txt", "public/l12"),
		"dotdot through blocked, missing":  f.symlink(f.blocked+"/nosuch/../../public/hello.txt", "public/l13"),
		"named like allowed, real blocked": f.symlink(f.path("private/secret.txt"), "public/fine.txt"),
	}
	for name, p := range cases {
		if _, err := f.check(p); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: Check(%s) = %v, want ErrBlocked", name, p, err)
		}
		if _, err := f.reader.Read(context.Background(), p, 0, 0); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: Read = %v, want ErrBlocked", name, err)
		}
	}
}

func TestSymlinkLoopAndMissing(t *testing.T) {
	f := newFixture(t)
	f.symlink(f.path("public/b"), "public/a")
	f.symlink(f.path("public/a"), "public/b")
	f.symlink("self", "public/self")
	for _, p := range []string{f.path("public/a"), f.path("public/self"), f.path("public/a/x"), f.path("public/nope"), f.path("public/hello.txt/x")} {
		if _, err := f.check(p); !errors.Is(err, ErrNotFound) {
			t.Errorf("Check(%s) = %v, want ErrNotFound", p, err)
		}
	}
}

func TestHardLinkToConfigRefused(t *testing.T) {
	f := newFixture(t)
	link := f.path("public/innocent.txt")
	if err := os.Link(f.cfg, link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reader.Read(context.Background(), link, 0, 0); !errors.Is(err, ErrBlocked) {
		t.Errorf("hard link to config: %v", err)
	}
	if _, err := f.reader.Grep(context.Background(), f.path("public"), "server"); err != nil {
		t.Fatal(err)
	} else if res, _ := f.reader.Grep(context.Background(), f.path("public"), "server"); len(res.Matches) != 0 {
		t.Errorf("grep read a hard link to the config: %+v", res.Matches)
	}
	// A hard link between two ordinary files is fine.
	other := f.path("public/hello2.txt")
	if err := os.Link(f.path("public/hello.txt"), other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reader.Read(context.Background(), other, 0, 0); err != nil {
		t.Errorf("ordinary hard link: %v", err)
	}
}

func TestExcludedFoldersUnknownRefusesEverything(t *testing.T) {
	f := newFixture(t)
	f.folders.err = excluded.ErrUnknown
	ctx := context.Background()
	if _, err := f.check(f.path("public/hello.txt")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Check: %v", err)
	}
	if _, err := f.reader.Read(ctx, f.path("public/hello.txt"), 0, 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Read: %v", err)
	}
	if _, err := f.reader.List(ctx, f.path("public")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("List: %v", err)
	}
	if _, err := f.reader.Find(ctx, f.path("public"), "*"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Find: %v", err)
	}
	if _, err := f.reader.Grep(ctx, f.path("public"), "hello"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Grep: %v", err)
	}
}

func TestSpecialFilesRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	fifo := f.path("public/fifo")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reader.Read(ctx, fifo, 0, 0); !errors.Is(err, ErrNotFile) {
		t.Errorf("FIFO: %v", err)
	}
	if _, err := f.reader.Grep(ctx, fifo, "x"); !errors.Is(err, ErrNotFile) {
		t.Errorf("grep FIFO: %v", err)
	}

	// Unix socket paths are short on macOS, so use a short directory.
	dir, err := os.MkdirTemp("/tmp", "lf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := f.reader.Read(ctx, sock, 0, 0); !errors.Is(err, ErrNotFile) {
		t.Errorf("socket: %v", err)
	}

	// A device is refused by type even when nothing blocks /dev.
	open := NewReader(NewGuard(Rules{}, nil), DefaultLimits())
	if _, err := open.Read(ctx, "/dev/null", 0, 0); !errors.Is(err, ErrNotFile) {
		t.Errorf("device: %v", err)
	}
	if _, err := f.reader.Read(ctx, "/dev/null", 0, 0); !errors.Is(err, ErrBlocked) {
		t.Errorf("device under /dev: %v", err)
	}
	if _, err := f.reader.Read(ctx, f.path("public"), 0, 0); !errors.Is(err, ErrNotFile) {
		t.Errorf("directory: %v", err)
	}
	if _, err := f.reader.List(ctx, f.path("public/hello.txt")); !errors.Is(err, ErrNotDir) {
		t.Errorf("list a file: %v", err)
	}
	if _, err := f.reader.Find(ctx, f.path("public/hello.txt"), "*"); !errors.Is(err, ErrNotDir) {
		t.Errorf("find in a file: %v", err)
	}
}

// open must refuse a path whose last component is a symlink even if a caller
// skipped resolution (the race resolve -> open).
func TestOpenRefusesSymlinks(t *testing.T) {
	f := newFixture(t)
	c, err := f.guard.session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	link := f.symlink(f.path("public/hello.txt"), "public/l")
	if _, _, err := c.open(link, kindFile); err == nil {
		t.Error("opened a symlink")
	}
	dirLink := f.symlink(f.path("public"), "dirlink")
	if fd, err := openNoFollow(dirLink+"/hello.txt", false); err == nil {
		unix.Close(fd)
		t.Error("openNoFollow followed a symlinked directory")
	}
}

func TestAgentHomes(t *testing.T) {
	dir := t.TempDir()
	group := filepath.Join(dir, "group")
	passwd := filepath.Join(dir, "passwd")
	os.WriteFile(group, []byte("root:x:0:\n# comment\nlatch-agents:x:1500:agent, helper,ghost,weird\nother:x:1600:bob\n"), 0o644)
	os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\nagent:x:1000:1000::/home/agent:/bin/sh\nprimary:x:1001:1500::/home/primary:/bin/sh\nbob:x:1002:1600::/home/bob:/bin/sh\nslash:x:1003:1500::/:/bin/sh\n"), 0o644)
	lookup := func(name string) (string, error) {
		switch name {
		case "agent":
			return "/home/agent", nil
		case "helper":
			return "/srv/helper", nil
		case "weird":
			return "relative/home", nil
		}
		return "", errors.New("unknown user")
	}
	homes, err := agentHomes("latch-agents", group, passwd, lookup)
	if strings.Join(homes, ",") != "/home/agent,/home/primary,/srv/helper" {
		t.Errorf("homes = %v", homes)
	}
	if err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "slash") || !strings.Contains(err.Error(), "weird") {
		t.Errorf("err = %v", err)
	}

	homes, err = agentHomes("no-such-group-xyz", group, passwd, lookup)
	if len(homes) != 0 || err == nil {
		t.Errorf("missing group: %v %v", homes, err)
	}
	homes, err = agentHomes("other", group, filepath.Join(dir, "missing"), lookup)
	if len(homes) != 0 || err == nil {
		t.Errorf("missing passwd: %v %v", homes, err)
	}
}
