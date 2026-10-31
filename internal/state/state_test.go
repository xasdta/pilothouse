package state

import (
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/api"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func hook(ago time.Duration, s api.State, k api.BlockKind) Evidence {
	return Evidence{At: t0.Add(-ago), Layer: api.LayerHook, State: s, Kind: k, Detail: "hook " + string(s)}
}

func osc(ago time.Duration, s api.State, k api.BlockKind) Evidence {
	return Evidence{At: t0.Add(-ago), Layer: api.LayerOSC, State: s, Kind: k, Detail: "osc " + string(s)}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name      string
		in        Input
		state     api.State
		kind      api.BlockKind
		rule      Rule
		disagrees bool
		fresh     []bool
	}{
		{
			name:  "no evidence is starting",
			in:    Input{HasHooks: true},
			state: api.StateStarting, rule: RuleStarting,
		},
		{
			name:  "exited beats everything",
			in:    Input{Exited: true, PendingPermission: true, HasHooks: true, Evidence: []Evidence{hook(0, api.StateWorking, "")}},
			state: api.StateExited, rule: RuleExited, fresh: []bool{true},
		},
		{
			name:  "pending permission beats hooks",
			in:    Input{PendingPermission: true, HasHooks: true, Evidence: []Evidence{hook(0, api.StateWaiting, "")}},
			state: api.StateBlocked, kind: api.BlockPermission, rule: RulePermission, fresh: []bool{true},
		},
		{
			name:  "pending question blocks on a question",
			in:    Input{PendingPermission: true, PendingKind: api.BlockQuestion, PermissionDetail: "Asks: Which database?", HasHooks: true, Evidence: []Evidence{hook(0, api.StateWorking, "")}},
			state: api.StateBlocked, kind: api.BlockQuestion, rule: RulePermission, fresh: []bool{true},
		},
		{
			name: "latest hook wins",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(10*time.Second, api.StateWorking, ""), hook(time.Second, api.StateWaiting, ""),
			}},
			state: api.StateWaiting, rule: RuleHook, fresh: []bool{false, true},
		},
		{
			name: "equal timestamps: later entry wins",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(time.Second, api.StateWorking, ""), hook(time.Second, api.StateWaiting, ""),
			}},
			state: api.StateWaiting, rule: RuleHook, fresh: []bool{false, true},
		},
		{
			name: "osc newer than hook wins",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(30*time.Second, api.StateWorking, ""), osc(time.Second, api.StateBlocked, api.BlockQuestion),
			}},
			state: api.StateBlocked, kind: api.BlockQuestion, rule: RuleOSC, fresh: []bool{true, true},
		},
		{
			name: "osc older than hook loses",
			in: Input{HasHooks: true, Evidence: []Evidence{
				osc(30*time.Second, api.StateWaiting, ""), hook(time.Second, api.StateWorking, ""),
			}},
			state: api.StateWorking, rule: RuleHook, fresh: []bool{true, true},
		},
		{
			name: "osc contradicting a hook under 2s older keeps the hook",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(1500*time.Millisecond, api.StateWorking, ""), osc(500*time.Millisecond, api.StateWaiting, ""),
			}},
			state: api.StateWorking, rule: RuleHookOverOSC, disagrees: true, fresh: []bool{true, true},
		},
		{
			name: "osc agreeing with a recent hook is just osc",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(1500*time.Millisecond, api.StateWorking, ""), osc(500*time.Millisecond, api.StateWorking, ""),
			}},
			state: api.StateWorking, rule: RuleOSC, fresh: []bool{true, true},
		},
		{
			name: "osc contradicting a hook 2s+ older wins",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(3*time.Second, api.StateWorking, ""), osc(500*time.Millisecond, api.StateWaiting, ""),
			}},
			state: api.StateWaiting, rule: RuleOSC, fresh: []bool{true, true},
		},
		{
			name: "no hook adapter: osc wins even when older",
			in: Input{HasHooks: false, Evidence: []Evidence{
				osc(30*time.Second, api.StateErrored, ""), hook(time.Second, api.StateWorking, ""),
			}},
			state: api.StateErrored, rule: RuleOSC, fresh: []bool{true, true},
		},
		{
			name:  "stale working hook is unknown",
			in:    Input{HasHooks: true, Evidence: []Evidence{hook(21*time.Minute, api.StateWorking, "")}},
			state: api.StateUnknown, rule: RuleStale, fresh: []bool{false},
		},
		{
			name: "stale working hook ignores older fresh-looking evidence",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(40*time.Minute, api.StateWaiting, ""), hook(21*time.Minute, api.StateWorking, ""),
			}},
			state: api.StateUnknown, rule: RuleStale, fresh: []bool{false, false},
		},
		{
			name:  "old waiting stays waiting",
			in:    Input{HasHooks: true, Evidence: []Evidence{hook(3*time.Hour, api.StateWaiting, "")}},
			state: api.StateWaiting, rule: RuleHook, fresh: []bool{true},
		},
		{
			name: "stale hook, fresh older osc: osc not newer than hook, so unknown",
			in: Input{HasHooks: true, Evidence: []Evidence{
				osc(25*time.Minute, api.StateWaiting, ""), hook(21*time.Minute, api.StateWorking, ""),
			}},
			state: api.StateUnknown, rule: RuleStale, fresh: []bool{true, false},
		},
		{
			name: "process-layer evidence alone is starting",
			in: Input{HasHooks: true, Evidence: []Evidence{
				{At: t0, Layer: api.LayerProcess, State: api.StateWorking},
			}},
			state: api.StateStarting, rule: RuleStarting, fresh: []bool{false},
		},
		{
			name:  "just started is starting",
			in:    Input{HasHooks: true, Evidence: []Evidence{hook(5*time.Second, api.StateStarting, "")}},
			state: api.StateStarting, rule: RuleHook, fresh: []bool{true},
		},
		{
			name:  "started and silent past the grace is waiting for a prompt",
			in:    Input{HasHooks: true, Evidence: []Evidence{hook(StartGrace+time.Second, api.StateStarting, "")}},
			state: api.StateWaiting, rule: RuleIdleStart, fresh: []bool{true},
		},
		{
			name:  "started days ago and never used is still waiting, not stale",
			in:    Input{HasHooks: true, Evidence: []Evidence{hook(48*time.Hour, api.StateStarting, "")}},
			state: api.StateWaiting, rule: RuleIdleStart, fresh: []bool{true},
		},
		{
			name: "work after a start wins",
			in: Input{HasHooks: true, Evidence: []Evidence{
				hook(time.Minute, api.StateStarting, ""), hook(time.Second, api.StateWorking, ""),
			}},
			state: api.StateWorking, rule: RuleHook, fresh: []bool{false, true},
		},
		{
			name: "pending permission beats an idle start",
			in: Input{PendingPermission: true, HasHooks: true, Evidence: []Evidence{
				hook(time.Hour, api.StateStarting, ""),
			}},
			state: api.StateBlocked, kind: api.BlockPermission, rule: RulePermission, fresh: []bool{true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Now = t0
			got := Resolve(tt.in)
			if got.State != tt.state || got.Kind != tt.kind || got.Rule != tt.rule {
				t.Fatalf("Resolve = %s/%s rule %s; want %s/%s rule %s", got.State, got.Kind, got.Rule, tt.state, tt.kind, tt.rule)
			}
			if (got.Disagreement != "") != tt.disagrees {
				t.Fatalf("disagreement = %q", got.Disagreement)
			}
			if tt.fresh != nil {
				for i := range tt.fresh {
					if got.Fresh[i] != tt.fresh[i] {
						t.Fatalf("fresh = %v, want %v", got.Fresh, tt.fresh)
					}
				}
			}
			if got.Detail == "" {
				t.Fatal("empty detail")
			}
		})
	}
}

func TestNeedsAttention(t *testing.T) {
	tests := []struct {
		ps   api.State
		pk   api.BlockKind
		s    api.State
		k    api.BlockKind
		want bool
	}{
		{api.StateWorking, "", api.StateWaiting, "", true},
		{api.StateWaiting, "", api.StateWaiting, "", false},
		{api.StateWorking, "", api.StateBlocked, api.BlockPermission, true},
		{api.StateBlocked, api.BlockPermission, api.StateBlocked, api.BlockQuestion, true},
		{api.StateBlocked, api.BlockPermission, api.StateBlocked, api.BlockPermission, false},
		{api.StateWaiting, "", api.StateWorking, "", false},
		{api.StateWorking, "", api.StateErrored, "", true},
		{api.StateWaiting, "", api.StateExited, "", true},
		{api.StateStarting, "", api.StateUnknown, "", false},
		{api.StateStarting, "", api.StateWaiting, "", false},
		{api.StateStarting, "", api.StateBlocked, api.BlockPermission, true},
	}
	for _, tt := range tests {
		if got := NeedsAttention(tt.ps, tt.pk, tt.s, tt.k); got != tt.want {
			t.Errorf("NeedsAttention(%s/%s → %s/%s) = %v", tt.ps, tt.pk, tt.s, tt.k, got)
		}
	}
}

func TestFromOSC(t *testing.T) {
	tests := []struct {
		s, k  string
		state api.State
		kind  api.BlockKind
		ok    bool
	}{
		{"idle", "", api.StateWaiting, "", true},
		{"done", "", api.StateWaiting, "", true},
		{"working", "", api.StateWorking, "", true},
		{"blocked", "auth", api.StateBlocked, api.BlockAuth, true},
		{"blocked", "", api.StateBlocked, api.BlockQuestion, true},
		{"error", "", api.StateErrored, "", true},
		{"dancing", "", "", "", false},
	}
	for _, tt := range tests {
		s, k, ok := FromOSC(tt.s, tt.k)
		if s != tt.state || k != tt.kind || ok != tt.ok {
			t.Errorf("FromOSC(%q,%q) = %s %s %v", tt.s, tt.k, s, k, ok)
		}
	}
}

func TestFromHook(t *testing.T) {
	tests := []struct {
		ev    adapters.HookEvent
		state api.State
		kind  api.BlockKind
		ok    bool
	}{
		{adapters.HookEvent{Kind: adapters.HookPreTool, Summary: "Run: ls"}, api.StateWorking, "", true},
		// A question tool about to wait for a person (Codex request_user_input
		// has no permission hook to show it otherwise).
		{adapters.HookEvent{Kind: adapters.HookPreTool, Summary: "Asks: Which database?", Questions: []api.Question{{Text: "Which database?", AllowOther: true}}}, api.StateBlocked, api.BlockQuestion, true},
		{adapters.HookEvent{Kind: adapters.HookPostTool}, api.StateWorking, "", true},
		{adapters.HookEvent{Kind: adapters.HookPromptSubmit}, api.StateWorking, "", true},
		{adapters.HookEvent{Kind: adapters.HookStop}, api.StateWaiting, "", true},
		{adapters.HookEvent{Kind: adapters.HookSessionEnd}, api.StateExited, "", true},
		{adapters.HookEvent{Kind: adapters.HookPermission, Summary: "Run: git push"}, api.StateBlocked, api.BlockPermission, true},
		{adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "permission_prompt"}, api.StateBlocked, api.BlockPermission, true},
		{adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "idle_prompt"}, api.StateWaiting, "", true},
		{adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "elicitation_dialog"}, api.StateBlocked, api.BlockQuestion, true},
		{adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "auth"}, api.StateBlocked, api.BlockAuth, true},
		{adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "auth_success"}, "", "", false},
		{adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "other"}, "", "", false},
		{adapters.HookEvent{Kind: adapters.HookUnknown}, "", "", false},
		// QA M1: subagent activity says nothing about the parent session.
		{adapters.HookEvent{Kind: adapters.HookSubagentStop}, "", "", false},
		{adapters.HookEvent{Kind: adapters.HookPreCompact}, api.StateWorking, "", true},
	}
	for _, tt := range tests {
		s, k, d, ok := FromHook(tt.ev)
		if s != tt.state || k != tt.kind || ok != tt.ok || (ok && d == "") {
			t.Errorf("FromHook(%s/%s) = %s %s %q %v", tt.ev.Kind, tt.ev.NotificationType, s, k, d, ok)
		}
	}
	if _, _, d, _ := FromHook(adapters.HookEvent{Kind: adapters.HookPermission, Summary: "Run: git push"}); d != "Asked to run: git push" {
		t.Errorf("permission detail = %q", d)
	}
}

func TestIsIdleNotification(t *testing.T) {
	if !IsIdleNotification(adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "idle_prompt"}) {
		t.Error("idle_prompt")
	}
	if IsIdleNotification(adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "permission_prompt"}) ||
		IsIdleNotification(adapters.HookEvent{Kind: adapters.HookStop, NotificationType: "idle_prompt"}) {
		t.Error("false positive")
	}
}
