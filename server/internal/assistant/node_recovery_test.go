package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/server/internal/prompts"
	"github.com/nwverse/persona/server/internal/proto"
)

type recoveryTestDevices struct {
	*fakeDevices
	mu        sync.Mutex
	machines  map[string]*proto.Machine
	connected map[string]time.Time
}

func newRecoveryTestDevices() *recoveryTestDevices {
	return &recoveryTestDevices{
		fakeDevices: &fakeDevices{},
		machines:    map[string]*proto.Machine{},
		connected:   map[string]time.Time{},
	}
}

func (d *recoveryTestDevices) setMachine(id, name, hostname string, bootTime int64, connectedAt time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.machines[id] = &proto.Machine{
		ID:       id,
		Name:     name,
		Hostname: hostname,
		Online:   true,
		System:   &proto.System{BootTime: bootTime},
	}
	d.connected[id] = connectedAt
}

func (d *recoveryTestDevices) MachineResources(id string) (*proto.Machine, *proto.MetricSample, *bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.machines[id], nil, nil
}

func (d *recoveryTestDevices) ConnectedAt(id string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected[id]
}

func createSupervisedRecoveryTodo(t *testing.T, s *Service, title, machine, session string) Todo {
	t.Helper()
	ctx := context.Background()
	todo, err := s.createTodo(ctx, title, "Recovery test task.", "conv1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.superviseTodo(ctx, todo.ID, "conv1", []string{"Checks pass"}, 30); err != nil {
		t.Fatal(err)
	}
	if err := s.linkTodoAgent(ctx, todo.ID, machine, session, title, "/code/app", "claude"); err != nil {
		t.Fatal(err)
	}
	return todoNow(s, todo.ID)
}

func TestNodeRecoveryDisconnectReconnect(t *testing.T) {
	ctx := context.Background()
	s, f := newQuietService(t)
	s.superviseTimes = superviseTimings{gather: time.Hour, every: time.Hour, urgent: time.Hour}
	devs := newRecoveryTestDevices()
	s.devs = devs

	base := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	bootBefore := base.Add(-2 * time.Hour).Unix()
	devs.setMachine("worker", "Worker Node", "worker-host", bootBefore, base.Add(-time.Hour))
	devs.setMachine("other", "Other Node", "other-host", bootBefore, base.Add(-time.Hour))

	// Record initial online state and then a disconnect.
	s.noteNodeOnline("worker", base)
	s.noteNodeOffline("worker", base)

	// Task 1: resumable Agent on the reconnecting node with a live tab and background monitor.
	tResumable := createSupervisedRecoveryTodo(t, s, "Run integration checks", "worker", "s-resumable")
	// Task 2: waiter Agent on another machine waiting on a CI job tied to the reconnecting node.
	tWaiter := createSupervisedRecoveryTodo(t, s, "Wait for CI on worker", "other", "s-waiter")
	// Task 3: unresumable Agent on the reconnecting node whose tab is closed.
	tClosed := createSupervisedRecoveryTodo(t, s, "Closed worker agent", "worker", "s-closed")
	// Task 4: Agent on the reconnecting node blocked on a needs_you card (cannot auto-wake).
	tNeedsYou := createSupervisedRecoveryTodo(t, s, "Card blocked worker agent", "worker", "s-needs-you")

	f.setSessions("worker",
		proto.Session{
			SessionID: "s-resumable", MachineID: "worker", Status: proto.StatusIdle, State: proto.StateIdle,
			PaneID: "w1:p1", StartedByPersona: true, UpdatedAt: base,
			Background: []proto.BackgroundTask{{ID: "bg1", Kind: "monitor", Description: "watch build queue"}},
		},
		proto.Session{
			SessionID: "s-closed", MachineID: "worker", Status: proto.StatusOffline, State: proto.StateOffline,
			StartedByPersona: true, UpdatedAt: base, Activity: "running migration",
		},
		proto.Session{
			SessionID: "s-needs-you", MachineID: "worker", Status: proto.StatusBlocked, State: proto.StateNeedsYou,
			PaneID: "w1:p2", PendingInteractions: 1, StartedByPersona: true, UpdatedAt: base,
			Activity: "awaiting approval for deploy",
		},
	)
	f.setSessions("other",
		proto.Session{
			SessionID: "s-waiter", MachineID: "other", Status: proto.StatusIdle, State: proto.StateWorking,
			PaneID: "w2:p1", StartedByPersona: true, UpdatedAt: base, BackgroundWaiting: true,
			Background: []proto.BackgroundTask{{
				ID: "bg-ci", Kind: "shell", Description: "CI shard on worker",
				Command: "ssh worker-host run-ci",
			}},
		},
	)

	var wakeMu sync.Mutex
	wakes := map[string]string{}
	f.answerNode(DeviceSessionSend, func(machine string, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		wakeMu.Lock()
		wakes[machine+"/"+p.SessionID] = p.Text
		wakeMu.Unlock()
		return map[string]any{"ok": true}, nil
	})

	// Brief disconnect (< nodeRecoveryOfflineAfter) with unchanged boot time does not trigger a sweep.
	shortBack := base.Add(nodeRecoveryOfflineAfter - time.Minute)
	devs.setMachine("worker", "Worker Node", "worker-host", bootBefore, shortBack)
	if res := s.nodeConnectedAt(ctx, "worker", shortBack); res.Triggered {
		t.Fatalf("brief disconnect should not trigger recovery sweep: %+v", res)
	}

	// Disconnect again and reconnect after an extended outage (20m > 10m).
	s.noteNodeOffline("worker", shortBack)
	reconnectedAt := shortBack.Add(20 * time.Minute)
	devs.setMachine("worker", "Worker Node", "worker-host", bootBefore, reconnectedAt)
	res := s.nodeConnectedAt(ctx, "worker", reconnectedAt)
	if !res.Triggered || res.Rebooted || res.Checked != 4 || res.Woken != 2 || res.Signalled != 2 {
		t.Fatalf("unexpected disconnect/reconnect sweep result: %+v", res)
	}

	// Resumable Agent on worker and waiter Agent on other should both be woken with outage details.
	wakeMu.Lock()
	resumableMsg := wakes["worker/s-resumable"]
	waiterMsg := wakes["other/s-waiter"]
	wakeMu.Unlock()
	if !strings.Contains(resumableMsg, "20 分钟") || !strings.Contains(resumableMsg, "watch build queue") {
		t.Fatalf("resumable wake message missing outage/step details: %q", resumableMsg)
	}
	if !strings.Contains(waiterMsg, "20 分钟") || !strings.Contains(waiterMsg, "CI shard on worker") {
		t.Fatalf("waiter wake message missing outage/lost job details: %q", waiterMsg)
	}

	// Unresumable closed Agent emits sigOrphaned; needs_you Agent emits sigRecovered.
	s.supervise.mu.Lock()
	gOrphan, okOrphan := s.supervise.pending[tClosed.ID+"|"+sigOrphaned]
	gRecovered, okRecovered := s.supervise.pending[tNeedsYou.ID+"|"+sigRecovered]
	_, hasResumableSig := s.supervise.pending[tResumable.ID+"|"+sigRecovered]
	_, hasWaiterSig := s.supervise.pending[tWaiter.ID+"|"+sigRecovered]
	s.supervise.mu.Unlock()

	if !okOrphan || gOrphan.Minutes != 20 || !strings.Contains(gOrphan.Detail, "running migration") || !strings.Contains(gOrphan.Detail, "20 分钟") {
		t.Fatalf("expected orphaned signal with outage and step, got %+v (ok=%v)", gOrphan, okOrphan)
	}
	if !okRecovered || gRecovered.Minutes != 20 || !strings.Contains(gRecovered.Detail, "awaiting approval for deploy") || !strings.Contains(gRecovered.Detail, "20 分钟") {
		t.Fatalf("expected recovered signal with outage and step, got %+v (ok=%v)", gRecovered, okRecovered)
	}
	if hasResumableSig || hasWaiterSig {
		t.Fatalf("resumed tasks should not emit unresumable signals")
	}

	// Both signals render into the supervision digest cleanly.
	eOrphan, ok := s.digestEntry(ctx, gOrphan, "")
	if !ok || eOrphan.Kind != sigOrphaned {
		t.Fatalf("digestEntry(orphaned) = %+v %v", eOrphan, ok)
	}
	eRecovered, ok := s.digestEntry(ctx, gRecovered, "")
	if !ok || eRecovered.Kind != sigRecovered || eRecovered.Minutes != 20 {
		t.Fatalf("digestEntry(recovered) = %+v %v", eRecovered, ok)
	}
	rendered := prompts.RenderSupervise(prompts.Supervise{Entries: []prompts.SuperviseEntry{eOrphan, eRecovered}})
	if !strings.Contains(rendered, "signal=orphaned") || !strings.Contains(rendered, "signal=recovered") {
		t.Fatalf("supervision digest missing signals:\n%s", rendered)
	}

	// Re-notifying the exact same connection must be idempotent.
	if dup := s.nodeConnectedAt(ctx, "worker", reconnectedAt.Add(time.Second)); dup.Triggered {
		t.Fatalf("duplicate notification for same connection should not re-sweep: %+v", dup)
	}
}

func TestNodeRecoveryReboot(t *testing.T) {
	ctx := context.Background()
	s, f := newQuietService(t)
	s.superviseTimes = superviseTimings{gather: time.Millisecond, every: time.Millisecond, urgent: time.Millisecond}
	devs := newRecoveryTestDevices()
	s.devs = devs

	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	oldBoot := base.Add(-4 * time.Hour).Unix()
	devs.setMachine("builder", "Builder Box", "builder-host", oldBoot, base.Add(-time.Hour))
	devs.setMachine("controller", "Controller Box", "controller-host", oldBoot, base.Add(-time.Hour))

	s.noteNodeOnline("builder", base)
	s.noteNodeOffline("builder", base)

	// Task 1: Agent on rebooted node became interrupted after reboot (cannot auto-resume).
	tInterrupted := createSupervisedRecoveryTodo(t, s, "Build release on builder", "builder", "s-interrupted")
	// Task 2: Waiter Agent on controller waiting on a CI job routed to builder.
	tWaiter := createSupervisedRecoveryTodo(t, s, "Run remote CI on builder", "controller", "s-ci-waiter")
	// Task 3: Agent whose wake fails falls back to sigRecovered.
	tWakeFail := createSupervisedRecoveryTodo(t, s, "Wake failure fallback", "builder", "s-wake-fail")

	f.setSessions("builder",
		proto.Session{
			SessionID: "s-interrupted", MachineID: "builder", Status: proto.StatusInterrupted, State: proto.StateUnknown,
			StartedByPersona: true, UpdatedAt: base, Plan: &proto.PlanInfo{Current: "compile server binary"},
		},
		proto.Session{
			SessionID: "s-wake-fail", MachineID: "builder", Status: proto.StatusIdle, State: proto.StateIdle,
			PaneID: "w1:p9", StartedByPersona: true, UpdatedAt: base, Activity: "syncing cache",
		},
	)
	f.setSessions("controller",
		proto.Session{
			SessionID: "s-ci-waiter", MachineID: "controller", Status: proto.StatusIdle, State: proto.StateWorking,
			PaneID: "w3:p1", StartedByPersona: true, UpdatedAt: base, BackgroundWaiting: true,
			Work: &proto.WorkState{Phase: proto.WorkBlocked, Wait: "waiting for CI job on builder-host"},
		},
	)

	var wakeMu sync.Mutex
	wakes := map[string]string{}
	f.answerNode(DeviceSessionSend, func(machine string, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "s-wake-fail" {
			return nil, errors.New("pane busy")
		}
		wakeMu.Lock()
		wakes[machine+"/"+p.SessionID] = p.Text
		wakeMu.Unlock()
		return map[string]any{"ok": true}, nil
	})

	// Only 2 minutes offline (< 10m), but BootTime is after lastOnline -> reboot detected!
	reconnectedAt := base.Add(2 * time.Minute)
	newBoot := base.Add(1 * time.Minute).Unix()
	devs.setMachine("builder", "Builder Box", "builder-host", newBoot, reconnectedAt)

	res := s.nodeConnectedAt(ctx, "builder", reconnectedAt)
	if !res.Triggered || !res.Rebooted || res.Checked != 3 || res.Woken != 1 || res.Signalled != 2 {
		t.Fatalf("unexpected reboot sweep result: %+v", res)
	}

	wakeMu.Lock()
	waiterMsg := wakes["controller/s-ci-waiter"]
	wakeMu.Unlock()
	if !strings.Contains(waiterMsg, "重启") || !strings.Contains(waiterMsg, "builder-host") {
		t.Fatalf("waiter wake message on reboot missing reboot/target info: %q", waiterMsg)
	}

	// Deliver digest and confirm signals go to supervisor silently (holdPushesParam=true, no owner push).
	s.sendDigest(ctx)
	notices := f.notices()
	if len(notices) != 1 {
		t.Fatalf("expected 1 supervision digest notice, got %d: %v", len(notices), notices)
	}
	msg := notices[0]
	for _, want := range []string{tInterrupted.ID, "signal=orphaned", "compile server binary", "重启", tWakeFail.ID, "signal=recovered", "syncing cache", "自动唤醒失败"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("supervision digest missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, tWaiter.ID) {
		t.Fatalf("successfully woken waiter should not be flagged as unresumable in digest:\n%s", msg)
	}

	// Verify the supervision message holds pushes (never pushes directly to the owner).
	f.mu.Lock()
	var convSends []map[string]any
	for _, a := range f.sends {
		if a["session_id"] == "conv1" {
			convSends = append(convSends, a)
		}
	}
	f.mu.Unlock()
	if len(convSends) != 1 || convSends[0][holdPushesParam] != true {
		t.Fatalf("supervision digest must hold owner pushes, got sends=%+v", convSends)
	}
}
