package cli

import (
	"context"
	"runtime"

	"github.com/xasdta/pilothouse/internal/config"
	"github.com/xasdta/pilothouse/internal/daemon"
)

// 0.2 workstream B: the keep-awake line of `pilothouse doctor`.

func init() { doctorKeepAwake = keepAwakeDoctor }

// keepAwakeDoctor reports keep_awake and whether this machine has the
// helper it needs.
func keepAwakeDoctor(_ context.Context, cfg *config.Config) *doctorCheck {
	st := daemon.KeepAwakeSupport(cfg.KeepAwake)
	c := &doctorCheck{Name: "keep-awake"}
	switch {
	case st.Mode == config.KeepAwakeOff:
		c.level, c.Detail = checkInfo, "off (keep_awake: off): the computer may idle-sleep while agents work"
	case st.Tool == "":
		need := "a sleep inhibitor"
		if runtime.GOOS == "linux" {
			need = "systemd-inhibit (systemd)"
		}
		c.level, c.Detail = checkInfo, "not available: keeping the computer awake needs "+need
	default:
		c.level, c.Detail = checkOK, "`"+st.Tool+"` while agents work or wait for you (idle sleep only; closing the lid still sleeps)"
	}
	c.Level = [...]string{"ok", "info", "warn", "fail"}[c.level]
	return c
}
