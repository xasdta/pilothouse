package opencode

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestPluginJS runs the plugin's own unit tests (plugin/pilothouse.test.mjs:
// a fake daemon and a fake OpenCode client) when Node.js is installed.
func TestPluginJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "plugin/pilothouse.test.mjs")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
