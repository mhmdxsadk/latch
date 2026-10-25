package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// volumeName matches Docker's volume name rule.
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

const maxVolumeBody = 1 << 20

// Exposure is everything a container exposes on the host that other tools
// must avoid when the container is excluded. Unlike Settings it is never cut
// to a display limit: a partial list would leave the rest reachable.
type Exposure struct {
	ID string
	// NetworkMode is "host" when the container shares the host's network,
	// so its listeners are the host's own ports.
	NetworkMode string
	Addrs       []string // IPv4 and IPv6 addresses on Docker networks
	HostPorts   []uint16 // ports published on the host
	// Folders are host folders mounted into the container (bind mounts and
	// local volumes that bind a host folder: driver_opts o=bind, device=...)
	// and the compose folder and files that define it.
	Folders []string
}

// Exposure returns what the container exposes. Volumes are looked up with
// GET /volumes/{name} to find bind-backed local volumes.
func (e *Engine) Exposure(ctx context.Context, container string) (Exposure, error) {
	in, err := e.inspect(ctx, container)
	if err != nil {
		return Exposure{}, err
	}
	x := Exposure{ID: in.ID, NetworkMode: in.HostConfig.NetworkMode}
	for _, n := range in.NetworkSettings.Networks {
		for _, ip := range []string{n.IPAddress, n.GlobalIPv6Address} {
			if ip != "" {
				x.Addrs = append(x.Addrs, ip)
			}
		}
	}
	for _, bindings := range in.NetworkSettings.Ports {
		for _, b := range bindings {
			if n, err := strconv.ParseUint(b.HostPort, 10, 16); err == nil && n > 0 {
				x.HostPorts = append(x.HostPorts, uint16(n))
			}
		}
	}
	for _, m := range in.Mounts {
		switch {
		case m.Type == "bind" && filepath.IsAbs(m.Source):
			x.Folders = append(x.Folders, filepath.Clean(m.Source))
		case m.Type == "volume" && m.Driver == "local" && m.Name != "":
			dev, err := e.volumeDevice(ctx, m.Name)
			if err != nil {
				return Exposure{}, err
			}
			if dev != "" {
				x.Folders = append(x.Folders, dev)
			}
		}
	}
	x.Folders = append(x.Folders, composePaths(in.Config.Labels)...)
	return x, nil
}

// composePaths returns the compose folder and files named in a container's
// labels, which often hold the container's secrets. Labels are written by
// whoever created the container, so each path is taken on its own and bad
// ones are skipped rather than failing the whole list. A compose folder of
// "/" is not returned: it would hide the whole disk, so only the files are.
func composePaths(labels map[string]string) []string {
	var out []string
	if wd := labels[labelWorkingDir]; cleanPath(wd) && wd != "/" {
		out = append(out, wd)
	}
	for f := range strings.SplitSeq(labels[labelConfigFiles], ",") {
		if cleanPath(f) {
			out = append(out, f)
		}
	}
	return out
}

// volumeDevice returns the host folder a local volume binds, or "".
func (e *Engine) volumeDevice(ctx context.Context, name string) (string, error) {
	if !volumeName.MatchString(name) {
		return "", errors.New("docker: invalid volume name")
	}
	resp, cancel, err := e.get(ctx, "/volumes/"+url.PathEscape(name), "")
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer cancel()
	defer resp.Body.Close()
	var v struct {
		Driver  string
		Options map[string]string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVolumeBody)).Decode(&v); err != nil {
		return "", errors.New("docker: malformed volume response")
	}
	dev := v.Options["device"]
	if v.Driver != "local" || !filepath.IsAbs(dev) {
		return "", nil
	}
	// Any local volume with an absolute device is a host path (o=bind) or a
	// block device that is mounted from it; block both.
	return filepath.Clean(dev), nil
}
