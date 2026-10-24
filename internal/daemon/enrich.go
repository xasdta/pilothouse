package daemon

import "github.com/xasdta/pilothouse/internal/api"

// enrichRequest adds what helps a person decide at a glance to a request
// about to be opened: risk labels, a preview of file changes and the
// agent's last words (F4; workstream D). It must be fast and never fail:
// on any problem it leaves the fields empty.
func (d *Daemon) enrichRequest(req *api.Request, h *hookCall) {}
