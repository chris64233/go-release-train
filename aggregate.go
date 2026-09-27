package releasetrain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// newID 生成短随机标识。
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newTrain(id, name string, now time.Time) *Train {
	return &Train{
		ID:         id,
		Name:       name,
		state:      StateEditing,
		Version:    1,
		candidates: map[string]Version{},
		createdAt:  now,
		updatedAt:  now,
	}
}

// requireState 断言当前状态。
func (t *Train) requireState(want ...TrainState) error {
	for _, s := range want {
		if t.state == s {
			return nil
		}
	}
	return fmt.Errorf("%w: train %s is %s, expected one of %v", ErrStateConflict, t.ID, t.state, want)
}

// SetCandidate 在冻结前设置/替换候选组件版本。冻结后任何修改都被拒绝。
func (t *Train) SetCandidate(component string, v Version, now time.Time) error {
	if err := t.requireState(StateEditing); err != nil {
		return err
	}
	t.candidates[component] = v
	t.Version++
	t.updatedAt = now
	return nil
}

// RemoveCandidate 在冻结前移除候选组件。
func (t *Train) RemoveCandidate(component string, now time.Time) error {
	if err := t.requireState(StateEditing); err != nil {
		return err
	}
	if _, ok := t.candidates[component]; !ok {
		return fmt.Errorf("%w: candidate %s not in train %s", ErrNotFound, component, t.ID)
	}
	delete(t.candidates, component)
	t.Version++
	t.updatedAt = now
	return nil
}

// Freeze 原子冻结：先完整校验快照，任一个问题都直接失败且不改任何状态；
// 校验通过后保存不可变候选快照与审批策略快照。
func (t *Train) Freeze(policy PolicySnapshot, reg versionLookup, now time.Time) error {
	if err := t.requireState(StateEditing); err != nil {
		return err
	}
	if len(t.candidates) == 0 {
		return fmt.Errorf("%w: cannot freeze train %s with no candidates", ErrInvalidArgument, t.ID)
	}
	if problems := validateSnapshot(t.candidates, reg); len(problems) > 0 {
		return &DependencyError{Problems: problems}
	}

	snap := cloneCandidates(t.candidates)
	t.frozenSnapshot = snap
	t.snapshotID = "snap_" + newID()
	t.frozenAt = now
	t.policy = &policy
	t.state = StateFrozen
	t.Version++
	t.updatedAt = now
	return nil
}

// eligibleRole 判断某人在策略快照中是否拥有指定角色。
func (p *PolicySnapshot) eligibleRole(person, role string) bool {
	for _, a := range p.Approvers {
		if a.Person == person && a.Role == role {
			return true
		}
	}
	return false
}

// hasApproval 判断 (person, role) 是否已审批。
func (t *Train) hasApproval(person, role string) bool {
	for _, a := range t.approvals {
		if a.Person == person && a.Role == role {
			return true
		}
	}
	return false
}

// Approve 记录一次审批。
//   - 仅 Frozen 状态可审批；
//   - 资格只看冻结时保存的策略快照，之后策略变更无影响；
//   - 同一 (person, role) 幂等：重复提交返回 false（表示未新增），不报错。
func (t *Train) Approve(person, role, requestID string, now time.Time) (added bool, err error) {
	if err := t.requireState(StateFrozen); err != nil {
		return false, err
	}
	if t.policy == nil || !t.policy.eligibleRole(person, role) {
		return false, fmt.Errorf("%w: %s is not an approver with role %q in the frozen policy of train %s",
			ErrApproval, person, role, t.ID)
	}
	if t.hasApproval(person, role) {
		return false, nil // 幂等：同人员同角色重复审批
	}
	t.approvals = append(t.approvals, Approval{Person: person, Role: role, At: now, RequestID: requestID})
	t.Version++
	t.updatedAt = now
	return true, nil
}

// approvalsMet 按策略快照统计每个角色的通过人数（同一人在该角色只计一次）。
// 返回未满足的规则描述列表，为空表示审批齐备。
func (t *Train) approvalsMet() []string {
	counts := map[string]map[string]struct{}{} // role -> set of persons
	for _, a := range t.approvals {
		if counts[a.Role] == nil {
			counts[a.Role] = map[string]struct{}{}
		}
		counts[a.Role][a.Person] = struct{}{}
	}
	var missing []string
	for _, rule := range t.policy.Rules {
		if got := len(counts[rule.Role]); got < rule.Need {
			missing = append(missing, fmt.Sprintf("role %q needs %d approval(s), got %d", rule.Role, rule.Need, got))
		}
	}
	sort.Strings(missing)
	return missing
}

// Cancel 取消列车。Editing 与 Frozen 可取消；终态下取消返回状态冲突。
func (t *Train) Cancel(now time.Time) error {
	if err := t.requireState(StateEditing, StateFrozen); err != nil {
		return err
	}
	t.state = StateCancelled
	t.cancelledAt = now
	t.Version++
	t.updatedAt = now
	return nil
}

// Release 放行列车：所有指定审批角色都通过后才能放行。
// 放行与取消互斥；调用方负责对“已放行重复调用”直接返回首次结果（幂等）。
// 放行在一个状态迁移中同时生成 outbox 事件，保证状态与事件一一对应。
func (t *Train) Release(now time.Time) (*ReleaseResult, *OutboxEvent, error) {
	if err := t.requireState(StateFrozen); err != nil {
		return nil, nil, err
	}
	if missing := t.approvalsMet(); len(missing) > 0 {
		return nil, nil, fmt.Errorf("%w: train %s cannot be released: %v", ErrApproval, t.ID, missing)
	}

	eventID := "evt_" + newID()
	payload, _ := json.Marshal(map[string]any{
		"train_id":    t.ID,
		"snapshot_id": t.snapshotID,
		"components":  t.frozenSnapshot,
	})

	t.state = StateReleased
	t.releasedAt = now
	t.releaseEventID = eventID
	t.Version++
	t.updatedAt = now

	event := &OutboxEvent{
		ID:        eventID,
		TrainID:   t.ID,
		Type:      "train.released",
		Payload:   json.RawMessage(payload),
		CreatedAt: now,
	}
	result := &ReleaseResult{
		TrainID:    t.ID,
		ReleasedAt: now,
		EventID:    eventID,
		SnapshotID: t.snapshotID,
	}
	return result, event, nil
}
