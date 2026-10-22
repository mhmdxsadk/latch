package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhmdxsadk/latch/internal/audit"
	"github.com/mhmdxsadk/latch/internal/config"
	"github.com/mhmdxsadk/latch/internal/host"
	"github.com/mhmdxsadk/latch/internal/policy"
)

type fakeHost struct {
	err error
}

func (f fakeHost) Load(context.Context) (host.Load, error) {
	return host.Load{Load1: 1, CPUs: 4}, f.err
}
func (f fakeHost) Memory(context.Context) (host.Memory, error) {
	return host.Memory{TotalBytes: 100, UsedBytes: 40, AvailableBytes: 60}, f.err
}
func (f fakeHost) Disk(_ context.Context, path string) (host.Disk, error) {
	if f.err != nil {
		return host.Disk{}, f.err
	}
	return host.Disk{TotalBytes: 1000, FreeBytes: 250, UsedBytes: 750}, nil
}

type harness struct {
	t     *testing.T
	cs    *sdk.ClientSession
	audit *bytes.Buffer
}

func newHarness(t *testing.T, h host.Provider) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server: {socket: /run/l.sock, allowed_uids: [1]}
host:
  disks:
    - {name: root, path: /}
docker:
  services:
    - {name: web, container: my-web}
`))
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	deps := &Deps{
		Policy:  policy.New(cfg),
		Host:    h,
		Audit:   audit.New(slog.New(slog.NewJSONHandler(buf, nil))),
		Version: "test",
	}
	srv := NewServer(deps)
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &harness{t: t, cs: cs, audit: buf}
}

// call returns the tool result; protocol-level errors are returned as err.
func (h *harness) call(name string, args any) (*sdk.CallToolResult, error) {
	h.t.Helper()
	return h.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
}

func resultText(r *sdk.CallToolResult) string {
	var sb strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolSurfaceIsReadOnlyAndFixed(t *testing.T) {
	h := newHarness(t, fakeHost{})
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_resources": true, "host_load": true, "host_memory": true, "host_disk": true}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q: v0.1 tools are an explicit list", tool.Name)
		}
		delete(want, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q must be annotated read-only", tool.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestListResources(t *testing.T) {
	h := newHarness(t, fakeHost{})
	res, err := h.call("list_resources", map[string]any{})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	var out listResourcesOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Disks) != 1 || out.Disks[0] != "root" || len(out.DockerServices) != 1 || out.SystemdUnits == nil {
		t.Errorf("got %+v", out)
	}
}

func TestHostDiskHappyPath(t *testing.T) {
	h := newHarness(t, fakeHost{})
	res, err := h.call("host_disk", map[string]any{"name": "root"})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(h.audit.String(), `"outcome":"ok"`) || !strings.Contains(h.audit.String(), `"resource":"root"`) {
		t.Errorf("audit missing: %s", h.audit)
	}
}

func TestHostDiskRejections(t *testing.T) {
	h := newHarness(t, fakeHost{})
	tests := []struct {
		name string
		args any
	}{
		{"unknown name", map[string]any{"name": "other"}},
		{"path instead of name", map[string]any{"name": "/"}},
		{"traversal", map[string]any{"name": "../etc"}},
		{"injection", map[string]any{"name": "root; cat /etc/shadow"}},
		{"empty", map[string]any{"name": ""}},
		{"missing name", map[string]any{}},
		{"wrong type", map[string]any{"name": 5}},
		{"null", map[string]any{"name": nil}},
		{"extra field smuggling a path", map[string]any{"name": "root", "path": "/etc"}},
		{"array instead of object", []any{"root"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := h.call("host_disk", tt.args)
			if err == nil && (res == nil || !res.IsError) {
				t.Fatalf("expected rejection, got %+v", res)
			}
		})
	}
}

func TestToolsRejectUnexpectedArguments(t *testing.T) {
	h := newHarness(t, fakeHost{})
	for _, tool := range []string{"host_load", "host_memory", "list_resources"} {
		res, err := h.call(tool, map[string]any{"path": "/etc/shadow"})
		if err == nil && !res.IsError {
			t.Errorf("%s accepted an unexpected argument", tool)
		}
	}
}

func TestUnknownToolRejected(t *testing.T) {
	h := newHarness(t, fakeHost{})
	for _, name := range []string{"exec", "docker_exec", "shell", "read_file", "restart_service"} {
		res, err := h.call(name, map[string]any{})
		if err == nil && !res.IsError {
			t.Errorf("tool %q must not exist", name)
		}
	}
}

// Integration errors may contain secrets or host details. They must reach
// neither the agent nor the audit log.
func TestIntegrationErrorsAreSanitised(t *testing.T) {
	secret := "SECRET-TOKEN-hunter2 /etc/shadow"
	h := newHarness(t, fakeHost{err: errors.New(secret)})
	for _, c := range []struct {
		tool string
		args any
	}{
		{"host_load", map[string]any{}},
		{"host_memory", map[string]any{}},
		{"host_disk", map[string]any{"name": "root"}},
	} {
		res, err := h.call(c.tool, c.args)
		var text string
		switch {
		case err != nil:
			text = err.Error()
		case res.IsError:
			text = resultText(res)
		default:
			t.Fatalf("%s: expected an error", c.tool)
		}
		if strings.Contains(text, "SECRET") || strings.Contains(text, "shadow") {
			t.Errorf("%s leaked integration error to agent: %q", c.tool, text)
		}
	}
	if a := h.audit.String(); strings.Contains(a, "SECRET") || strings.Contains(a, "shadow") {
		t.Errorf("integration error leaked into audit log: %s", a)
	}
	if !strings.Contains(h.audit.String(), `"reason":"integration_error"`) {
		t.Errorf("audit should record the category: %s", h.audit)
	}
}

func TestDeniedCallsAreAuditedWithoutEchoingInput(t *testing.T) {
	h := newHarness(t, fakeHost{})
	h.call("host_disk", map[string]any{"name": "evil-name"})
	h.call("host_disk", map[string]any{"name": "bad name;id"})
	a := h.audit.String()
	if strings.Contains(a, "evil-name") || strings.Contains(a, "bad name") {
		t.Errorf("audit log echoed untrusted input: %s", a)
	}
	if !strings.Contains(a, `"reason":"not_allowed"`) || !strings.Contains(a, `"reason":"invalid_name"`) {
		t.Errorf("expected denial reasons: %s", a)
	}
}

func TestUnsupportedPlatformMessage(t *testing.T) {
	h := newHarness(t, fakeHost{err: host.ErrUnsupported})
	res, err := h.call("host_load", map[string]any{})
	if err == nil && !res.IsError {
		t.Fatal("expected error")
	}
}
