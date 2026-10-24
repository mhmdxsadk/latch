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
)

type fakeDocker struct {
	docker.Client
	err   error
	calls int
}

func (f *fakeDocker) List(context.Context) ([]string, error) {
	f.calls++
	return []string{"vault", "web", "gone"}, f.err
}

func (f *fakeDocker) Settings(_ context.Context, name string) (docker.Settings, error) {
	switch name {
	case "vault":
		return docker.Settings{
			Ports:    []docker.Port{{Container: "8200/tcp", HostIP: "0.0.0.0", HostPort: "8200"}, {HostPort: "8200"}},
			Networks: []docker.Network{{Name: "n", IPAddress: "172.18.0.9", IPv6Address: "fd00::9"}},
			Mounts:   []docker.Mount{{Type: "bind", Source: "/srv/vault/data/"}, {Type: "volume", Source: "v"}},
		}, nil
	case "gone":
		return docker.Settings{}, docker.ErrNotFound
	}
	panic("visible container inspected: " + name)
}

func newTracker(t *testing.T, fd *fakeDocker, now *time.Time) *Tracker {
	t.Helper()
	cfg, err := config.Parse([]byte("server: {socket: /run/l.sock, socket_group: latch}\ndocker: {exclude: [vault, gone]}\n"))
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
