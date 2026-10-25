package excluded

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/docker"
	"github.com/mhmdxsadk/latch/internal/policy"
	"github.com/mhmdxsadk/latch/internal/ports"
)

type fakeDocker struct {
	docker.Client
	err   error
	calls int
	extra []string
}

type fakePorts struct {
	err error
}

func (f fakePorts) Listening(context.Context) ([]ports.Listener, error) {
	return []ports.Listener{{Port: 9000, ContainerID: "hostnetid"}, {Port: 22}, {Port: 9001, ContainerID: "otherid"}}, f.err
}

func (f *fakeDocker) List(context.Context) ([]string, error) {
	f.calls++
	return append([]string{"vault", "web", "gone"}, f.extra...), f.err
}

func (f *fakeDocker) Exposure(_ context.Context, name string) (docker.Exposure, error) {
	switch name {
	case "vault":
		return docker.Exposure{
			ID:        "vaultid",
			HostPorts: []uint16{8200, 8200},
			Addrs:     []string{"172.18.0.9", "fd00::9"},
			Folders:   []string{"/srv/vault/data"},
		}, nil
	case "gone":
		return docker.Exposure{}, docker.ErrNotFound
	case "hostnet":
		return docker.Exposure{ID: "hostnetid", NetworkMode: "host"}, nil
	}
	panic("visible container inspected: " + name)
}

func newTracker(t *testing.T, fd *fakeDocker, now *time.Time) *Tracker {
	t.Helper()
	cfg, err := config.Parse([]byte("server: {socket: /run/l.sock, socket_group: latch}\ndocker: {exclude: [vault, gone, hostnet]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &Tracker{Docker: fd, Policy: policy.New(cfg), Now: func() time.Time { return *now }}
}

func TestSnapshot(t *testing.T) {
	now := time.Unix(1000, 0)
	fd := &fakeDocker{}
	tr := newTracker(t, fd, &now)
	s, err := tr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasAddr(netip.MustParseAddr("172.18.0.9")) || !s.HasAddr(netip.MustParseAddr("::ffff:172.18.0.9")) ||
		!s.HasAddr(netip.MustParseAddr("fd00::9")) || s.HasAddr(netip.MustParseAddr("172.18.0.10")) {
		t.Errorf("addrs %v", s.Addrs)
	}
	if !slices.Equal(s.Ports, []uint16{8200}) || !slices.Equal(s.Folders, []string{"/srv/vault/data"}) {
		t.Errorf("ports %v folders %v", s.Ports, s.Folders)
	}

	// Cached within TTL, refreshed after it.
	tr.Get(context.Background())
	if fd.calls != 1 {
		t.Errorf("calls %d", fd.calls)
	}
	now = now.Add(TTL)
	tr.Get(context.Background())
	if fd.calls != 2 {
		t.Errorf("calls %d", fd.calls)
	}
}

func TestFailsClosedWithoutSnapshot(t *testing.T) {
	now := time.Unix(1000, 0)
	fd := &fakeDocker{err: errors.New("down")}
	tr := newTracker(t, fd, &now)
	if _, err := tr.Get(context.Background()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("got %v", err)
	}
	fd.err = nil
	if _, err := tr.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Docker goes down later: the last good snapshot is kept.
	fd.err = errors.New("down")
	now = now.Add(time.Hour)
	if s, err := tr.Get(context.Background()); err != nil || len(s.Folders) != 1 {
		t.Errorf("got %+v %v", s, err)
	}
}

// An excluded container on the host network publishes nothing, so its
// listening ports come from the socket table; without one, fail closed.
func TestHostNetworkContainer(t *testing.T) {
	now := time.Unix(1000, 0)
	fd := &fakeDocker{extra: []string{"hostnet"}}
	tr := newTracker(t, fd, &now)
	if _, err := tr.Get(context.Background()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("without a scanner: %v", err)
	}
	tr.Ports = fakePorts{err: errors.New("no proc")}
	if _, err := tr.Get(context.Background()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("scanner failing: %v", err)
	}
	tr.Ports = fakePorts{}
	s, err := tr.Get(context.Background())
	if err != nil || !slices.Equal(s.Ports, []uint16{8200, 9000}) {
		t.Errorf("ports %v %v", s.Ports, err)
	}
}
