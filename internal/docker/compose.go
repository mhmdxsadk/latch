package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mhmdxsadk/latch/internal/redact"
	"github.com/mhmdxsadk/latch/internal/textsafe"
)

// Compose labels that docker compose puts on every container it creates.
const (
	labelProject     = "com.docker.compose.project"
	labelService     = "com.docker.compose.service"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
)

const (
	// MaxComposeFiles bounds the -f arguments.
	MaxComposeFiles = 8
	// ComposeTimeout bounds one "docker compose up" run, including pulls.
	ComposeTimeout = 10 * time.Minute
	// ComposeOutputLines is how many output lines are returned, from the end.
	ComposeOutputLines = 40

	maxPathLen        = 4096
	maxComposeLine    = 512
	maxComposeCapture = 256 << 10
)

var (
	// composeProject is docker compose's project name rule.
	composeProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
	// composeService is the compose spec's service name rule, additionally
	// starting with a letter or digit so it can never read as a flag.
	composeService = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

// DockerCLIPaths are the only places the docker CLI is run from.
var DockerCLIPaths = []string{"/usr/bin/docker", "/usr/local/bin/docker"}

// Errors for compose targets. Their text is safe to show.
var (
	ErrNotCompose    = errors.New("the container was not created by docker compose")
	ErrBadCompose    = errors.New("the container's compose labels are not usable, or its compose files are missing")
	ErrComposeFailed = errors.New("docker compose failed")
	ErrNoDockerCLI   = errors.New("the docker CLI was not found in " + strings.Join(DockerCLIPaths, " or "))
)

// ComposeTarget is one compose service, read from a container's labels.
type ComposeTarget struct {
	Project     string   `json:"project"`
	Service     string   `json:"service"`
	WorkingDir  string   `json:"working_dir"`
	ConfigFiles []string `json:"config_files"`
	// Siblings are the names of every container of this project and service
	// (filled by Actions.ComposeTarget).
	Siblings []string `json:"siblings,omitempty"`
}

// Same reports whether t and o name the same service with the same files.
func (t ComposeTarget) Same(o ComposeTarget) bool {
	return t.Project == o.Project && t.Service == o.Service && t.WorkingDir == o.WorkingDir &&
		slices.Equal(t.ConfigFiles, o.ConfigFiles)
}

func (t ComposeTarget) labels() map[string]string {
	return map[string]string{
		labelProject:     t.Project,
		labelService:     t.Service,
		labelWorkingDir:  t.WorkingDir,
		labelConfigFiles: strings.Join(t.ConfigFiles, ","),
	}
}

// ComposeResult is the outcome of "docker compose up".
type ComposeResult struct {
	ExitCode int      `json:"exit_code"`
	TimedOut bool     `json:"timed_out"`
	Output   []string `json:"output"` // last lines, sanitized and redacted
}

// ParseComposeLabels validates the compose labels of a container. Nothing
// else from the container is used to build the command.
func ParseComposeLabels(labels map[string]string) (ComposeTarget, error) {
	project, okP := labels[labelProject]
	service, okS := labels[labelService]
	wd, okW := labels[labelWorkingDir]
	files, okF := labels[labelConfigFiles]
	if !okP && !okS && !okW && !okF {
		return ComposeTarget{}, ErrNotCompose
	}
	if !composeProject.MatchString(project) || !composeService.MatchString(service) || !cleanPath(wd) || files == "" {
		return ComposeTarget{}, ErrBadCompose
	}
	t := ComposeTarget{Project: project, Service: service, WorkingDir: wd}
	for f := range strings.SplitSeq(files, ",") {
		if !cleanPath(f) {
			return ComposeTarget{}, ErrBadCompose
		}
		t.ConfigFiles = append(t.ConfigFiles, f)
	}
	if len(t.ConfigFiles) > MaxComposeFiles {
		return ComposeTarget{}, ErrBadCompose
	}
	return t, nil
}

// cleanPath accepts clean absolute paths of printable characters.
func cleanPath(p string) bool {
	return p != "" && len(p) <= maxPathLen && filepath.IsAbs(p) && filepath.Clean(p) == p &&
		textsafe.Line(p) == p && !strings.ContainsAny(p, "\t,")
}

// CheckComposeFiles requires the working directory to be a directory and
// every config file to be a regular file (symlinks are followed).
func CheckComposeFiles(t ComposeTarget, stat func(string) (fs.FileInfo, error)) error {
	if fi, err := stat(t.WorkingDir); err != nil || !fi.IsDir() {
		return ErrBadCompose
	}
	if len(t.ConfigFiles) == 0 || len(t.ConfigFiles) > MaxComposeFiles {
		return ErrBadCompose
	}
	for _, f := range t.ConfigFiles {
		if fi, err := stat(f); err != nil || !fi.Mode().IsRegular() {
			return ErrBadCompose
		}
	}
	return nil
}

func osStat(p string) (fs.FileInfo, error) { return os.Stat(p) }

// ComposeArgs is the exact argument list given to the docker CLI. Every value
// comes from a validated ComposeTarget; nothing comes from the agent except
// the pull choice.
func ComposeArgs(t ComposeTarget, pull bool) []string {
	args := []string{"compose", "--project-name", t.Project, "--project-directory", t.WorkingDir}
	for _, f := range t.ConfigFiles {
		args = append(args, "-f", f)
	}
	args = append(args, "up", "-d", "--no-deps")
	if pull {
		args = append(args, "--pull", "always")
	}
	return append(args, t.Service)
}

// ComposeEnv is the whole environment of the docker CLI.
func ComposeEnv(socketPath string) []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"DOCKER_HOST=unix://" + socketPath,
	}
}

// FindDockerCLI returns the first of paths that is a regular file owned by
// root and not writable by group or others.
func FindDockerCLI(paths []string) (string, error) {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
			continue
		}
		return p, nil
	}
	return "", ErrNoDockerCLI
}

// composeRunner runs the docker CLI; tests substitute a fake.
type composeRunner interface {
	run(ctx context.Context, dir string, args []string) (ComposeResult, error)
}

type execRunner struct {
	socket string
	paths  []string
}

func newExecRunner(socket string) execRunner {
	return execRunner{socket: socket, paths: DockerCLIPaths}
}

func (r execRunner) run(ctx context.Context, dir string, args []string) (ComposeResult, error) {
	bin, err := FindDockerCLI(r.paths)
	if err != nil {
		return ComposeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ComposeTimeout)
	defer cancel()
	out := &tailBuffer{max: maxComposeCapture}
	code, err := runCLI(ctx, bin, dir, args, r.socket, out)
	res := ComposeResult{ExitCode: code, Output: outputTail(out.String(), ComposeOutputLines)}
	switch {
	case ctx.Err() != nil:
		res.TimedOut = true
		return res, ErrComposeFailed
	case err != nil:
		return res, err
	case code != 0:
		return res, ErrComposeFailed
	}
	return res, nil
}

// runCLI runs bin with args, no shell, a fixed environment and no stdin, in
// its own process group so a timeout kills the compose plugin too.
func runCLI(ctx context.Context, bin, dir string, args []string, socket string, out *tailBuffer) (int, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is from DockerCLIPaths, args from ComposeArgs or fixed
	cmd.Dir = dir
	cmd.Env = ComposeEnv(socket)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	default:
		return -1, errors.New("docker compose could not be run")
	}
}

// CheckComposeCLI reports which docker CLI would run compose, after checking
// that "docker compose version" works. doctor uses it.
func CheckComposeCLI(ctx context.Context, socketPath string) (string, error) {
	bin, err := FindDockerCLI(DockerCLIPaths)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := &tailBuffer{max: 4096}
	code, err := runCLI(ctx, bin, "/", []string{"compose", "version"}, socketPath, out)
	if err != nil || code != 0 {
		return bin, fmt.Errorf("%s compose version failed; is the docker compose plugin installed?", bin)
	}
	return bin, nil
}

// outputTail sanitizes and redacts captured output and keeps the last n
// non-empty lines.
func outputTail(s string, n int) []string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = textsafe.Head(textsafe.Line(strings.TrimSuffix(l, "\r")), maxComposeLine)
	}
	// Redact the whole text so a private key spanning lines is caught.
	lines = strings.Split(redact.Text(strings.Join(lines, "\n")), "\n")
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i])
		}
	}
	slices.Reverse(out)
	return out
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= t.max {
		t.b = append(t.b[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.b) + len(p) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	t.b = append(t.b, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.b), "�")
}
