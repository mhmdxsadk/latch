package policy

import (
	"testing"

	"github.com/mhmdxsadk/latch/internal/config"
)

func TestApproveLists(t *testing.T) {
	cfg, err := config.Parse([]byte(`
server: {socket: /run/l.sock, socket_group: latch-agents}
docker: {approve: [db-*]}
systemd: {approve: [cron.service]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AdminSocket != config.DefaultAdminSocket {
		t.Errorf("admin socket default = %q", cfg.Server.AdminSocket)
	}
	p := New(cfg)
	for name, want := range map[string]bool{"db-main": true, "db": false, "web": false} {
		if got := p.ContainerNeedsApproval(name); got != want {
			t.Errorf("container %s: %v", name, got)
		}
	}
	for name, want := range map[string]bool{
		"cron.service": true, "nginx.service": false,
		"docker.service": true, "ssh.service": true, "sshd.service": true,
		"systemd-networkd.service": true, "networking.service": true, "latch.service": true,
		"systemd-resolved.service": true, "docker.socket": true, "containerd.service": true, "dbus.service": true,
		// Only services restart automatically; other unit types change many
		// services at once.
		"cron.timer": true, "shutdown.target": true, "reboot.target": true, "nginx.socket": true, "srv.mount": true,
		"nginx@a.service": false,
	} {
		if got := p.UnitNeedsApproval(name); got != want {
			t.Errorf("unit %s: %v", name, got)
		}
	}
	// The built-in list applies with no config at all and cannot be removed.
	empty, _ := config.Parse([]byte("server: {socket: /run/l.sock, socket_group: latch-agents}\n"))
	if !New(empty).UnitNeedsApproval("ssh.service") {
		t.Error("built-in approve list missing")
	}
	BuiltinApproveUnits[0] = "x" // New copied it; existing policies are unaffected
	defer func() { BuiltinApproveUnits[0] = "docker.service" }()
	if !p.UnitNeedsApproval("docker.service") {
		t.Error("policy shares the built-in slice")
	}
}
