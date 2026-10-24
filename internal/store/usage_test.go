package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/policy"
)

func TestUsageUpsertKeepsTheLargerCount(t *testing.T) {
	db := openTest(t)
	row := UsageRow{SessionID: "s_1", Day: "2026-10-23", Agent: api.AgentClaude, Model: "m", Repo: "/src/a",
		Input: 100, Output: 10, CacheRead: 1000, CacheWrite: 50, Reasoning: 5, CostUSD: 1.25, Turns: 3}
	upsert := func(rows ...UsageRow) {
		t.Helper()
		if err := db.Write(ctx, func(tx *Tx) error { return tx.UpsertUsage(ctx, rows) }); err != nil {
			t.Fatal(err)
		}
	}
	upsert(row, UsageRow{SessionID: "s_1", Day: "2026-10-22", Agent: api.AgentClaude, Model: "m", Input: 1, CostUSD: 0.5},
		UsageRow{SessionID: "s_2", Day: "2026-10-23", Agent: api.AgentCodex, Model: "g", Input: 7, CostUSD: 2},
		UsageRow{Day: "2026-10-23"}, UsageRow{SessionID: "s_3"}) // incomplete rows are skipped

	// The same totals again change nothing; a smaller re-count (a
	// transcript read from its tail) can't lower them; growth is kept;
	// an empty repo doesn't erase the known one.
	upsert(row)
	smaller := row
	smaller.Input, smaller.CostUSD, smaller.Repo = 10, 0.1, ""
	upsert(smaller)
	grown := row
	grown.Output, grown.Turns = 20, 4
	upsert(grown)

	got, err := db.UsageDays(ctx, "2026-10-23", "2026-10-23")
	if err != nil {
		t.Fatal(err)
	}
	want := row
	want.Output, want.Turns = 20, 4
	if len(got) != 2 || !reflect.DeepEqual(got[0], want) || got[1].SessionID != "s_2" {
		t.Errorf("rows = %+v\nwant %+v", got, want)
	}
	if all, _ := db.UsageDays(ctx, "2026-10-01", "2026-10-31"); len(all) != 3 || all[0].Day != "2026-10-22" {
		t.Errorf("range rows = %+v", all)
	}
	if spent, err := db.SpentOn(ctx, "2026-10-23"); err != nil || spent != 1.25+2 {
		t.Errorf("spent = %v %v", spent, err)
	}
	if spent, err := db.SpentOn(ctx, "2020-01-01"); err != nil || spent != 0 {
		t.Errorf("spent on an empty day = %v %v", spent, err)
	}
}

func TestDecisionRows(t *testing.T) {
	db := openTest(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := base
	db.SetClock(func() time.Time { return clock })
	created := base.Add(-time.Minute)
	closed := base
	emit := func(at time.Time, typ api.EventType, data any) {
		t.Helper()
		clock = at
		if err := db.Write(ctx, func(tx *Tx) error { _, err := tx.Emit(ctx, typ, "s_1", "", data); return err }); err != nil {
			t.Fatal(err)
		}
	}
	emit(base.Add(-48*time.Hour), api.EventPolicyDecision, policy.DecisionEvent{Tool: "Bash", Result: policy.Result{Action: api.BehaviorDeny, Rule: "old"}, Outcome: api.BehaviorDeny})
	emit(base, api.EventPolicyDecision, policy.DecisionEvent{Tool: "Bash", Summary: "Run: rm -rf /",
		Result: policy.Result{Action: api.BehaviorDeny, Rule: "no-rm"}, Outcome: api.BehaviorDeny})
	emit(base, api.EventPolicyDecision, policy.DecisionEvent{Tool: "Bash", Grant: "g_1", Outcome: api.BehaviorAllow})
	big := api.Request{ID: "r_1", Status: api.RequestOpen, Checkpoint: api.CheckpointPermission, CreatedAt: created,
		Input: []byte(`{"content":"` + strings.Repeat("x", 100_000) + `"}`)}
	emit(base, api.EventRequestOpened, big)
	done := big
	done.Status, done.ClosedAt = api.RequestDecided, &closed
	done.Decision = &api.Decision{Behavior: api.BehaviorAllow, By: "device:d_1"}
	emit(base.Add(time.Second), api.EventRequestClosed, done)
	emit(base, api.EventSessionUpdated, api.Session{ID: "s_1"}) // other types are left out
	emit(base, api.EventHook, nil)

	rows, err := db.DecisionRows(ctx, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Type != api.EventPolicyDecision || r.Outcome != api.BehaviorDeny || r.Rule != "no-rm" || r.Summary != "Run: rm -rf /" || !r.At.Equal(base) {
		t.Errorf("deny row = %+v", r)
	}
	if r := rows[1]; r.Grant != "g_1" || r.Outcome != api.BehaviorAllow {
		t.Errorf("grant row = %+v", r)
	}
	if r := rows[2]; r.Type != api.EventRequestOpened || r.Status != api.RequestOpen || r.Checkpoint != api.CheckpointPermission || !r.CreatedAt.Equal(created) {
		t.Errorf("opened row = %+v", r)
	}
	if r := rows[3]; r.Status != api.RequestDecided || r.By != "device:d_1" || !r.ClosedAt.Equal(closed) || !r.CreatedAt.Equal(created) {
		t.Errorf("closed row = %+v", r)
	}
	if none, _ := db.DecisionRows(ctx, base.Add(time.Hour), base.Add(2*time.Hour)); len(none) != 0 {
		t.Errorf("empty range returned %d rows", len(none))
	}
}

func TestSessionEventsOfType(t *testing.T) {
	db := openTest(t)
	for i := 0; i < 5; i++ {
		typ := api.EventSessionUpdated
		if i%2 == 0 {
			typ = api.EventSessionReply
		}
		if err := db.Write(ctx, func(tx *Tx) error {
			_, err := tx.Emit(ctx, typ, "s_1", "", api.ReplyRecord{Text: string(rune('a' + i))})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := db.SessionEventsOfType(ctx, "s_1", api.EventSessionReply, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Seq <= evs[1].Seq || evs[0].Type != api.EventSessionReply {
		t.Errorf("events = %+v", evs)
	}
}
