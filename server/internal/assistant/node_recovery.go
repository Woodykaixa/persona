package assistant

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/server/internal/proto"
)

// Node recovery sweep (#14781): when a node reconnects after an extended outage
// or after a host reboot (reported BootTime later than its last online time),
// inspect open supervised tasks tied to that node — both tasks whose Agents run
// on it and tasks whose Agents on other nodes wait on background commands, CI
// jobs or Monitors targeting it. Resumable Agents are woken automatically with
// a note explaining what may have been lost; unresumable tasks emit an orphaned
// or recovered supervision signal (into the supervision digest, never pushed to
// the owner) carrying the outage duration and affected step.

var (
	// nodeRecoveryOfflineAfter is the minimum outage duration before a reconnect
	// triggers a recovery sweep (aligned with orphanOfflineAfter; tests shorten it).
	nodeRecoveryOfflineAfter = 10 * time.Minute
)

const nodeRecoveryKeyPrefix = "node-recovery:"

type nodeOnlineRecord struct {
	lastOnline  time.Time
	offlineAt   time.Time
	bootTime    int64
	sweptConnAt time.Time
	sweptBootAt int64
}

type nodeRecoveryBook struct {
	mu      sync.Mutex
	records map[string]nodeOnlineRecord
}

type nodeRecoveryEvent struct {
	machine     string
	machineName string
	hostname    string
	outage      time.Duration
	rebooted    bool
	connectedAt time.Time
	bootTime    int64
}

// nodeRecoveryResult summarizes one sweep for logging and unit tests.
type nodeRecoveryResult struct {
	Triggered  bool
	Rebooted   bool
	Outage     time.Duration
	Checked    int
	Woken      int
	Signalled  int
	WokenTodos []string
}

func (s *Service) noteNodeOnline(machine string, now time.Time) {
	id := s.canonical(machine)
	if id == "" {
		return
	}
	var bootTime int64
	if s.devs != nil {
		if m, _, _ := s.devs.MachineResources(id); m != nil && m.System != nil {
			bootTime = m.System.BootTime
		}
	}
	b := &s.nodeRecovery
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.records == nil {
		b.records = map[string]nodeOnlineRecord{}
	}
	rec := b.records[id]
	if now.After(rec.lastOnline) {
		rec.lastOnline = now
	}
	rec.offlineAt = time.Time{}
	if bootTime > 0 && rec.bootTime == 0 {
		rec.bootTime = bootTime
	}
	b.records[id] = rec
}

func (s *Service) noteNodeOffline(machine string, now time.Time) {
	id := s.canonical(machine)
	if id == "" {
		return
	}
	b := &s.nodeRecovery
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.records == nil {
		b.records = map[string]nodeOnlineRecord{}
	}
	rec := b.records[id]
	if rec.offlineAt.IsZero() {
		rec.offlineAt = now
	}
	if rec.lastOnline.IsZero() {
		rec.lastOnline = now
	}
	b.records[id] = rec
}

// NodeConnected is called when a node finishes connecting and listing sessions
// (devices.Options.OnConnected). If the node was offline longer than
// nodeRecoveryOfflineAfter or rebooted since it was last online, it runs a
// recovery sweep for affected open tasks.
func (s *Service) NodeConnected(ctx context.Context, machine string) {
	s.nodeConnectedAt(ctx, machine, time.Now())
}

func (s *Service) nodeConnectedAt(ctx context.Context, machine string, now time.Time) nodeRecoveryResult {
	id := s.canonical(machine)
	if id == "" {
		return nodeRecoveryResult{}
	}
	var (
		connectedAt time.Time
		bootTime    int64
		machineName = id
		hostname    string
	)
	if s.devs != nil {
		connectedAt = s.devs.ConnectedAt(id)
		if m, _, _ := s.devs.MachineResources(id); m != nil {
			if m.Name != "" {
				machineName = m.Name
			}
			hostname = m.Hostname
			if m.System != nil {
				bootTime = m.System.BootTime
			}
		}
	}
	if machineName == id {
		if d, ok := s.device(ctx, id); ok && d.Name != "" {
			machineName = d.Name
		}
	}
	if connectedAt.IsZero() {
		connectedAt = now
	}

	b := &s.nodeRecovery
	b.mu.Lock()
	if b.records == nil {
		b.records = map[string]nodeOnlineRecord{}
	}
	rec, had := b.records[id]
	prevOnline := rec.lastOnline
	if !rec.offlineAt.IsZero() && (prevOnline.IsZero() || rec.offlineAt.After(prevOnline)) {
		prevOnline = rec.offlineAt
	}
	prevBoot := rec.bootTime

	var outage time.Duration
	if !prevOnline.IsZero() && now.After(prevOnline) {
		outage = now.Sub(prevOnline)
	}
	rebooted := false
	if bootTime > 0 {
		if !prevOnline.IsZero() && time.Unix(bootTime, 0).After(prevOnline) {
			rebooted = true
		} else if prevBoot > 0 && bootTime > prevBoot {
			rebooted = true
		}
	}
	alreadySwept := !rec.sweptConnAt.IsZero() && rec.sweptConnAt.Equal(connectedAt) && rec.sweptBootAt == bootTime
	shouldSweep := had && !alreadySwept && (rebooted || outage >= nodeRecoveryOfflineAfter)

	rec.lastOnline = now
	rec.offlineAt = time.Time{}
	if bootTime > 0 {
		rec.bootTime = bootTime
	}
	if shouldSweep {
		rec.sweptConnAt = connectedAt
		rec.sweptBootAt = bootTime
	}
	b.records[id] = rec
	b.mu.Unlock()

	if !shouldSweep {
		return nodeRecoveryResult{Rebooted: rebooted, Outage: outage}
	}
	ev := nodeRecoveryEvent{
		machine:     id,
		machineName: machineName,
		hostname:    hostname,
		outage:      outage,
		rebooted:    rebooted,
		connectedAt: connectedAt,
		bootTime:    bootTime,
	}
	return s.runNodeRecoverySweep(ctx, ev, now)
}

func (s *Service) runNodeRecoverySweep(ctx context.Context, ev nodeRecoveryEvent, now time.Time) nodeRecoveryResult {
	res := nodeRecoveryResult{
		Triggered: true,
		Rebooted:  ev.rebooted,
		Outage:    ev.outage,
	}
	if err := s.ensureTodosLoaded(ctx); err != nil {
		return res
	}
	tasks := s.todosWhere(ctx, "*", func(t *Todo) bool {
		if t.DoneAt != nil || t.Supervision == nil || len(t.Agents) == 0 {
			return false
		}
		switch t.Supervision.state(*t) {
		case reviewClosing, reviewTimedOut, reviewUserCheck, reviewPending:
			return false
		}
		return true
	})
	if len(tasks) == 0 {
		return res
	}

	lists := machineSessions{}
	tokens := nodeMatchTokens(ev.machine, ev.machineName, ev.hostname)
	outageMin := max(1, int((ev.outage+30*time.Second)/time.Minute))
	if ev.outage <= 0 {
		outageMin = 0
	}

	for _, t := range tasks {
		active := activeTodoAgents(t)
		if len(active) == 0 {
			continue
		}
		type target struct {
			agent    TodoAgent
			session  proto.Session
			listed   bool
			isWaiter bool
			step     string
		}
		var affected []target
		allAgents := s.taskAgentsIn(ctx, t, lists)
		byKey := make(map[[2]string]proto.Session, len(allAgents))
		for _, c := range allAgents {
			byKey[[2]string{s.canonical(c.MachineID), c.SessionID}] = c
		}
		for _, a := range active {
			am := s.canonical(a.MachineID)
			c, listed := byKey[[2]string{am, a.SessionID}]
			if am == ev.machine {
				affected = append(affected, target{
					agent:    a,
					session:  c,
					listed:   listed,
					isWaiter: false,
					step:     affectedStepSummary(t, c, ev.machineName, false),
				})
				continue
			}
			if listed {
				if waitStep, ok := waitingOnNodeStep(c, tokens); ok {
					affected = append(affected, target{
						agent:    a,
						session:  c,
						listed:   true,
						isWaiter: true,
						step:     waitStep,
					})
				}
			}
		}
		if len(affected) == 0 {
			continue
		}
		res.Checked++

		var (
			wokenAny       bool
			unresumable    []string
			unresumableRef TodoAgent
			allGone        = true
			orphanReason   = orphanClosed
		)
		for _, a := range active {
			am := s.canonical(a.MachineID)
			c, listed := byKey[[2]string{am, a.SessionID}]
			if listed && sessionResumableAfterOutage(c) {
				allGone = false
			}
		}
		for _, item := range affected {
			if item.listed && sessionResumableAfterOutage(item.session) {
				wakeText := formatRecoveryWakeMessage(ev, outageMin, item.step, item.isWaiter)
				sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := s.call(sctx, item.agent.MachineID, DeviceSessionSend, sendParams(item.agent.SessionID, wakeText, false), nil)
				cancel()
				if err == nil {
					wokenAny = true
					res.Woken++
					continue
				}
				unresumableRef = item.agent
				unresumable = append(unresumable, fmt.Sprintf("%s（自动唤醒失败：%v）", item.step, err))
				continue
			}
			unresumableRef = item.agent
			why := unresumableReason(item.listed, item.session, ev.rebooted)
			if item.listed && item.session.TurnError != "" && !proto.SilentTurnKind(item.session.TurnErrorKind) {
				orphanReason = orphanError
			}
			unresumable = append(unresumable, fmt.Sprintf("%s（%s）", item.step, why))
		}
		if wokenAny {
			res.WokenTodos = append(res.WokenTodos, t.ID)
		}
		if len(unresumable) == 0 {
			continue
		}
		detail := formatRecoverySignalDetail(ev, outageMin, unresumable)
		key := fmt.Sprintf("%s%s@%s", nodeRecoveryKeyPrefix, ev.machine, ev.connectedAt.UTC().Format(time.RFC3339))
		if ev.bootTime > 0 {
			key += fmt.Sprintf("#%d", ev.bootTime)
		}
		kind := sigRecovered
		if allGone {
			kind = sigOrphaned
		}
		sig := superviseSignal{
			Todo:    t.ID,
			Kind:    kind,
			Key:     key,
			At:      now,
			Machine: cmp.Or(unresumableRef.MachineID, ev.machine),
			Session: unresumableRef.SessionID,
			Minutes: outageMin,
			Detail:  detail,
		}
		if kind == sigOrphaned {
			sig.Orphan = orphanReason
		}
		s.signal(sig)
		res.Signalled++
	}
	s.logf("node recovery sweep %s (rebooted=%v, outage=%s): checked=%d woken=%d signalled=%d",
		ev.machine, ev.rebooted, ev.outage.Round(time.Second), res.Checked, res.Woken, res.Signalled)
	return res
}

func activeTodoAgents(t Todo) []TodoAgent {
	var out []TodoAgent
	for _, a := range t.Agents {
		if a.ReleasedAt == nil && a.DismissedAt == nil {
			out = append(out, a)
		}
	}
	return out
}

// sessionResumableAfterOutage reports whether a listed session still has a live
// terminal/host process and can safely accept an automatic recovery wake.
func sessionResumableAfterOutage(c proto.Session) bool {
	if c.Assistant || c.Starting {
		return true
	}
	if sessionClosed(c) {
		return false
	}
	if c.Status == proto.StatusOffline || c.Status == proto.StatusInterrupted {
		return false
	}
	if c.State == proto.StateNeedsYou || c.PendingInteractions > 0 {
		return false
	}
	if c.TurnError != "" && c.TurnErrorKind != proto.TurnErrorTransient && !proto.SilentTurnKind(c.TurnErrorKind) {
		return false
	}
	return true
}

func unresumableReason(listed bool, c proto.Session, rebooted bool) string {
	switch {
	case !listed:
		if rebooted {
			return "节点重启后会话已消失"
		}
		return "节点重连后未列出该会话"
	case c.Status == proto.StatusInterrupted:
		return "节点重启导致终端进程中断"
	case sessionClosed(c) || c.Status == proto.StatusOffline:
		return "Agent 标签或进程已关闭"
	case c.State == proto.StateNeedsYou || c.PendingInteractions > 0:
		return "Agent 正在等待交互卡片确认，无法自动唤醒"
	case c.TurnError != "":
		return "Agent 上一轮报错停止：" + clip(oneLine(c.TurnError), 120)
	default:
		return "Agent 当前不可自动续接"
	}
}

func nodeMatchTokens(machineID, machineName, hostname string) []string {
	var out []string
	for _, raw := range []string{machineID, machineName, hostname} {
		v := strings.ToLower(strings.TrimSpace(raw))
		if len(v) < 2 || slices.Contains(out, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func matchesNodeTokens(text string, tokens []string) bool {
	if text == "" || len(tokens) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, tok := range tokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// waitingOnNodeStep checks whether an Agent running on another machine is
// waiting on a background command, CI job or Monitor tied to the reconnected node.
func waitingOnNodeStep(c proto.Session, tokens []string) (string, bool) {
	if len(tokens) == 0 {
		return "", false
	}
	for _, bg := range c.Background {
		if matchesNodeTokens(bg.Command, tokens) || matchesNodeTokens(bg.Description, tokens) {
			label := cmp.Or(strings.TrimSpace(bg.Description), strings.TrimSpace(bg.Command), bg.Kind)
			return fmt.Sprintf("后台 %s「%s」", cmp.Or(bg.Kind, "任务"), clip(oneLine(label), 120)), true
		}
	}
	if c.Work != nil && (c.Work.Phase == proto.WorkBlocked || c.Work.Wait != "" || c.Work.Line != "") {
		if matchesNodeTokens(c.Work.Wait, tokens) || matchesNodeTokens(c.Work.Line, tokens) {
			label := cmp.Or(strings.TrimSpace(c.Work.Wait), strings.TrimSpace(c.Work.Line))
			return fmt.Sprintf("等待外部步骤「%s」", clip(oneLine(label), 120)), true
		}
	}
	if matchesNodeTokens(c.Activity, tokens) && (c.BackgroundWorking || c.BackgroundWaiting || c.State == proto.StateWorking) {
		return fmt.Sprintf("当前步骤「%s」", clip(oneLine(c.Activity), 120)), true
	}
	return "", false
}

func affectedStepSummary(t Todo, c proto.Session, machineName string, isWaiter bool) string {
	if len(c.Background) > 0 {
		bg := c.Background[0]
		label := cmp.Or(strings.TrimSpace(bg.Description), strings.TrimSpace(bg.Command), bg.Kind)
		return fmt.Sprintf("后台 %s「%s」", cmp.Or(bg.Kind, "任务"), clip(oneLine(label), 120))
	}
	if c.Work != nil && (c.Work.Wait != "" || c.Work.Line != "") {
		return clip(oneLine(cmp.Or(c.Work.Wait, c.Work.Line)), 120)
	}
	if c.Plan != nil && strings.TrimSpace(c.Plan.Current) != "" {
		return clip(oneLine(c.Plan.Current), 120)
	}
	if strings.TrimSpace(c.Activity) != "" {
		return clip(oneLine(c.Activity), 120)
	}
	if strings.TrimSpace(t.Stage) != "" {
		return "阶段 " + t.Stage
	}
	if strings.TrimSpace(c.LastText) != "" {
		return clip(oneLine(c.LastText), 120)
	}
	if isWaiter {
		return fmt.Sprintf("等待节点 %s 的后台作业", machineName)
	}
	return fmt.Sprintf("节点 %s 上的进行中步骤", machineName)
}

func formatRecoveryWakeMessage(ev nodeRecoveryEvent, outageMin int, step string, isWaiter bool) string {
	cause := fmt.Sprintf("离线 %d 分钟后已重新连上", outageMin)
	if ev.rebooted {
		if outageMin > 0 {
			cause = fmt.Sprintf("离线 %d 分钟并发生重启后已恢复上线", outageMin)
		} else {
			cause = "刚刚发生重启并已恢复上线"
		}
	}
	if isWaiter {
		return fmt.Sprintf("【节点恢复通知】目标节点 %s %s。你正在等待的 %s 可能在断连/重启期间中断，输出结果已丢失。请立即核对远端状态，必要时重新提交作业并继续推进当前任务。",
			ev.machineName, cause, step)
	}
	return fmt.Sprintf("【节点恢复通知】所在节点 %s %s。你手上的 %s 及相关后台命令、Monitor 或临时连接可能已在断连期间中断或丢失。请检查现场状态，重新提交丢失的步骤并继续完成任务。",
		ev.machineName, cause, step)
}

func formatRecoverySignalDetail(ev nodeRecoveryEvent, outageMin int, items []string) string {
	eventDesc := fmt.Sprintf("节点 %s 离线 %d 分钟后重连", ev.machineName, outageMin)
	if ev.rebooted {
		if outageMin > 0 {
			eventDesc = fmt.Sprintf("节点 %s 离线 %d 分钟并重启后恢复", ev.machineName, outageMin)
		} else {
			eventDesc = fmt.Sprintf("节点 %s 重启后恢复", ev.machineName)
		}
	}
	return fmt.Sprintf("%s；受影响步骤：%s", eventDesc, strings.Join(items, "；"))
}
