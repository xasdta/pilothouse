package cli

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/xasdta/pilothouse/internal/config"
)

func TestKeepAwakeDoctor(t *testing.T) {
	cfg := config.Default()
	cfg.KeepAwake = config.KeepAwakeOff
	if c := keepAwakeDoctor(context.Background(), cfg); c.level != checkInfo || !strings.Contains(c.Detail, "keep_awake: off") {
		t.Fatalf("off: %+v", c)
	}
	cfg.KeepAwake = ""
	c := keepAwakeDoctor(context.Background(), cfg)
	_, caffeinate := exec.LookPath("caffeinate")
	switch {
	case runtime.GOOS == "darwin" && caffeinate == nil:
		if c.level != checkOK || c.Level != "ok" || !strings.Contains(c.Detail, "caffeinate -i") {
			t.Fatalf("macOS: %+v", c)
		}
	case c.level == checkOK:
		if !strings.Contains(c.Detail, "systemd-inhibit") {
			t.Fatalf("ok without a known tool: %+v", c)
		}
	default:
		if c.level != checkInfo || !strings.Contains(c.Detail, "not available") {
			t.Fatalf("no tool: %+v", c)
		}
	}
}
