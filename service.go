package releasetrain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Service 是发布列车的应用服务。所有写操作都在单个 Store.Update 事务内完成：
// 业务校验失败时事务整体回滚，不留下部分结果。
type Service struct {
	store Store
	now   func() time.Time
}

// NewService 创建服务。store 通常为 NewFileStore 的返回值。
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// registryAdapter 让 storedState 实现 validation 需要的 versionLookup。
type registryAdapter struct {
	reg map[string]map[string]ComponentVersion
}

func (r registryAdapter) constraintsOf(component string, v Version) ([]Constraint, bool) {
	if m, ok := r.reg[component]; ok {
		if rec, ok := m[v.String()]; ok {
			return rec.Constraints, true
		}
	}
	return nil, false
}

// ---- 幂等支撑 ----

// idempotency 处理同一 requestID 的重放。
// 返回 (replayed=true, raw) 表示命中历史结果；同键不同指纹/种类直接冲突。
func checkIdem(recs map[string]idemRecord, requestID, kind, fingerprint string) (bool, json.RawMessage, error) {
	if requestID == "" {
		return false, nil, nil
	}
	if rec, ok := recs[requestID]; ok {
		if rec.Kind != kind || rec.Fingerprint != fingerprint {
			return false, nil, fmt.Errorf("%w: request id %q was already used with a different payload",
				ErrIdempotencyConflict, requestID)
		}
		return true, rec.Result, nil
	}
	return false, nil, nil
}

func saveIdem(recs map[string]idemRecord, requestID, kind, trainID, fingerprint string, result any) error {
	if requestID == "" {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	recs[requestID] = idemRecord{Kind: kind, TrainID: trainID, Fingerprint: fingerprint, Result: raw}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func fingerprint(v any) string { return string(mustJSON(v)) }

// checkVersion 校验乐观版本条件。expectedVersion <= 0 表示不检查。
func checkVersion(t *Train, expected int) error {
	if expected > 0 && t.Version != expected {
		return fmt.Errorf("%w: train %s version is %d but request expected %d",
			ErrVersionConflict, t.ID, t.Version, expected)
	}
	return nil
}

func canonConstraints(cs []Constraint) []Constraint {
	out := append([]Constraint(nil), cs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		return out[i].Version.Compare(out[j].Version) < 0
	})
	return out
}

// GetPolicy 返回当前审批策略（未设置时为 nil）。
func (s *Service) GetPolicy() *PolicySnapshot {
	var out *PolicySnapshot
	_ = s.store.View(func(st *storedState) error {
		if st.policy != nil {
			p := *st.policy
			p.Rules = append([]ApprovalRule(nil), st.policy.Rules...)
			p.Approvers = append([]Approver(nil), st.policy.Approvers...)
			out = &p
		}
		return nil
	})
	return out
}

// ---- 版本登记 ----

// RegisterVersionInput 是登记组件版本的请求。
type RegisterVersionInput struct {
	Component   string
	Version     Version
	Constraints []Constraint
	RequestID   string
}

// RegisterVersion 登记一个不可变的组件版本及其对其他组件的版本约束。
// 同一 (组件, 版本) 重复登记且约束一致时幂等返回；约束不一致返回 ErrIdempotencyConflict。
func (s *Service) RegisterVersion(in RegisterVersionInput) (*ComponentVersion, error) {
	if strings.TrimSpace(in.Component) == "" {
		return nil, fmt.Errorf("%w: component name is required", ErrInvalidArgument)
	}
	cs := canonConstraints(in.Constraints)
	for _, c := range cs {
		if strings.TrimSpace(c.Component) == "" {
			return nil, fmt.Errorf("%w: constraint component is required", ErrInvalidArgument)
		}
		if !validOp(c.Op) {
			return nil, fmt.Errorf("%w: unknown constraint op %q", ErrInvalidArgument, c.Op)
		}
	}
	fp := fingerprint(struct {
		Component string       `json:"component"`
		Version   Version      `json:"version"`
		Cons      []Constraint `json:"constraints"`
	}{in.Component, in.Version, cs})

	var out *ComponentVersion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, in.RequestID, "register_version", fp); err != nil {
			return err
		} else if replayed {
			var rec ComponentVersion
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			out = &rec
			return nil
		}

		byComp := st.versions[in.Component]
		if byComp == nil {
			byComp = map[string]ComponentVersion{}
			st.versions[in.Component] = byComp
		}
		key := in.Version.String()
		if existing, ok := byComp[key]; ok {
			// 版本不可变：约束必须逐字一致。
			if !constraintsEqual(existing.Constraints, cs) {
				return fmt.Errorf("%w: component %s version %s is already registered with different constraints",
					ErrIdempotencyConflict, in.Component, key)
			}
			rec := existing
			out = &rec
			return saveIdem(st.idem, in.RequestID, "register_version", "", fp, existing)
		}

		rec := ComponentVersion{
			Component:   in.Component,
			Version:     in.Version,
			Constraints: cs,
			CreatedAt:   s.now().UTC(),
		}
		byComp[key] = rec
		out = &rec
		return saveIdem(st.idem, in.RequestID, "register_version", "", fp, rec)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func constraintsEqual(a, b []Constraint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- 审批策略 ----

// PutPolicy 设置当前审批策略。只影响此后冻结的列车；
// 已冻结列车使用其冻结时的策略快照，不受影响。
func (s *Service) PutPolicy(rules []ApprovalRule, approvers []Approver) error {
	if err := validatePolicy(rules, approvers); err != nil {
		return err
	}
	p := &PolicySnapshot{Rules: append([]ApprovalRule(nil), rules...), Approvers: append([]Approver(nil), approvers...)}
	return s.store.Update(func(st *storedState) error {
		st.policy = p
		return nil
	})
}

// validatePolicy 校验策略：角色规则不重复、need 为正、人员不重复且角色有对应规则、
// 每个角色的有资格人数不少于需求。
func validatePolicy(rules []ApprovalRule, approvers []Approver) error {
	needs := map[string]int{}
	for _, r := range rules {
		if strings.TrimSpace(r.Role) == "" {
			return fmt.Errorf("%w: approval rule role is required", ErrInvalidArgument)
		}
		if r.Need <= 0 {
			return fmt.Errorf("%w: approval rule %q need must be positive", ErrInvalidArgument, r.Role)
		}
		if _, dup := needs[r.Role]; dup {
			return fmt.Errorf("%w: duplicate approval rule for role %q", ErrInvalidArgument, r.Role)
		}
		needs[r.Role] = r.Need
	}
	seatCount := map[string]map[string]struct{}{}
	seen := map[string]struct{}{}
	for _, a := range approvers {
		if strings.TrimSpace(a.Person) == "" || strings.TrimSpace(a.Role) == "" {
			return fmt.Errorf("%w: approver person and role are required", ErrInvalidArgument)
		}
		if _, ok := needs[a.Role]; !ok {
			return fmt.Errorf("%w: approver %s has role %q without a matching rule", ErrInvalidArgument, a.Person, a.Role)
		}
		pk := a.Person + "\x00" + a.Role
		if _, dup := seen[pk]; dup {
			return fmt.Errorf("%w: approver %s listed twice for role %q", ErrInvalidArgument, a.Person, a.Role)
		}
		seen[pk] = struct{}{}
		if seatCount[a.Role] == nil {
			seatCount[a.Role] = map[string]struct{}{}
		}
		seatCount[a.Role][a.Person] = struct{}{}
	}
	for role, need := range needs {
		if got := len(seatCount[role]); got < need {
			return fmt.Errorf("%w: role %q needs %d approver(s) but policy only lists %d",
				ErrInvalidArgument, role, need, got)
		}
	}
	return nil
}

// ---- 列车编辑 ----

// CreateTrain 创建一列编辑态列车。
func (s *Service) CreateTrain(name, requestID string) (*Train, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: train name is required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		Name string `json:"name"`
	}{name})

	var out *Train
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "create_train", fp); err != nil {
			return err
		} else if replayed {
			out = decodeTrain(raw)
			return nil
		}
		t := newTrain("train_"+newID(), name, s.now().UTC())
		st.trains[t.ID] = t
		out = cloneTrain(t)
		return saveIdem(st.idem, requestID, "create_train", t.ID, fp, toWire(t))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func decodeTrain(raw json.RawMessage) *Train {
	var w trainWire
	if err := json.Unmarshal(raw, &w); err != nil {
		panic(err) // 我们自己写入的数据，损坏属于编程错误
	}
	return fromWire(&w)
}

// SetCandidateInput 设置/替换候选组件。
type SetCandidateInput struct {
	TrainID         string
	Component       string
	Version         Version
	ExpectedVersion int // 乐观条件；<=0 表示不检查
	RequestID       string
}

// SetCandidate 在冻结前向列车加入或替换候选组件版本。
func (s *Service) SetCandidate(in SetCandidateInput) (*Train, error) {
	if strings.TrimSpace(in.Component) == "" {
		return nil, fmt.Errorf("%w: component name is required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		TrainID   string  `json:"train_id"`
		Component string  `json:"component"`
		Version   Version `json:"version"`
	}{in.TrainID, in.Component, in.Version})

	var out *Train
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, in.RequestID, "set_candidate", fp); err != nil {
			return err
		} else if replayed {
			out = decodeTrain(raw)
			return nil
		}
		t, err := requireTrain(st, in.TrainID)
		if err != nil {
			return err
		}
		if err := checkVersion(t, in.ExpectedVersion); err != nil {
			return err
		}
		// 候选版本必须是已登记版本（组件版本登记后才允许上车）。
		reg := registryAdapter{st.versions}
		if _, ok := reg.constraintsOf(in.Component, in.Version); !ok {
			return fmt.Errorf("%w: component %s version %s is not registered",
				ErrDependency, in.Component, in.Version)
		}
		if err := t.SetCandidate(in.Component, in.Version, s.now().UTC()); err != nil {
			return err
		}
		out = cloneTrain(t)
		return saveIdem(st.idem, in.RequestID, "set_candidate", t.ID, fp, toWire(t))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveCandidate 在冻结前移除候选组件。
func (s *Service) RemoveCandidate(trainID, component string, expectedVersion int, requestID string) (*Train, error) {
	fp := fingerprint(struct {
		TrainID   string `json:"train_id"`
		Component string `json:"component"`
	}{trainID, component})

	var out *Train
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "remove_candidate", fp); err != nil {
			return err
		} else if replayed {
			out = decodeTrain(raw)
			return nil
		}
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}
		if err := checkVersion(t, expectedVersion); err != nil {
			return err
		}
		if err := t.RemoveCandidate(component, s.now().UTC()); err != nil {
			return err
		}
		out = cloneTrain(t)
		return saveIdem(st.idem, requestID, "remove_candidate", t.ID, fp, toWire(t))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func requireTrain(st *storedState, id string) (*Train, error) {
	t, ok := st.trains[id]
	if !ok {
		return nil, fmt.Errorf("%w: train %s", ErrNotFound, id)
	}
	return t, nil
}

// ---- 冻结 ----

// Freeze 冻结列车：原子校验依赖图无环且全部版本约束在候选快照内可满足，
// 并把当前审批策略复制为不可变的策略快照。任一组件不兼容则整次冻结失败、无部分结果。
func (s *Service) Freeze(trainID string, expectedVersion int, requestID string) (*Train, error) {
	fp := fingerprint(struct {
		TrainID string `json:"train_id"`
	}{trainID})

	var out *Train
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "freeze", fp); err != nil {
			return err
		} else if replayed {
			out = decodeTrain(raw)
			return nil
		}
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}
		if err := checkVersion(t, expectedVersion); err != nil {
			return err
		}
		if st.policy == nil {
			return fmt.Errorf("%w: no approval policy configured; freeze train %s requires one",
				ErrInvalidArgument, trainID)
		}
		// 深拷贝策略快照，之后当前策略如何变化都与本列车无关。
		snap := PolicySnapshot{
			Rules:     append([]ApprovalRule(nil), st.policy.Rules...),
			Approvers: append([]Approver(nil), st.policy.Approvers...),
		}
		if err := t.Freeze(snap, registryAdapter{st.versions}, s.now().UTC()); err != nil {
			return err
		}
		out = cloneTrain(t)
		return saveIdem(st.idem, requestID, "freeze", t.ID, fp, toWire(t))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 审批 ----

// Approve 以指定人员和角色审批冻结列车。资格来自冻结时的策略快照；
// 同一 (person, role) 幂等。
func (s *Service) Approve(trainID, person, role string, expectedVersion int, requestID string) (*Train, error) {
	if strings.TrimSpace(person) == "" || strings.TrimSpace(role) == "" {
		return nil, fmt.Errorf("%w: person and role are required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		TrainID string `json:"train_id"`
		Person  string `json:"person"`
		Role    string `json:"role"`
	}{trainID, person, role})

	var out *Train
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "approve", fp); err != nil {
			return err
		} else if replayed {
			out = decodeTrain(raw)
			return nil
		}
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}
		if err := checkVersion(t, expectedVersion); err != nil {
			return err
		}
		if _, err := t.Approve(person, role, requestID, s.now().UTC()); err != nil {
			return err
		}
		out = cloneTrain(t)
		return saveIdem(st.idem, requestID, "approve", t.ID, fp, toWire(t))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 取消 / 放行 ----

// Cancel 取消列车。Editing/Frozen 可取消；已放行后取消返回 ErrStateConflict。
// 重复取消幂等返回当前列车。
func (s *Service) Cancel(trainID string, expectedVersion int, requestID string) (*Train, error) {
	fp := fingerprint(struct {
		TrainID string `json:"train_id"`
	}{trainID})

	var out *Train
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "cancel", fp); err != nil {
			return err
		} else if replayed {
			out = decodeTrain(raw)
			return nil
		}
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}
		if err := checkVersion(t, expectedVersion); err != nil {
			return err
		}
		if t.State() == StateCancelled {
			out = cloneTrain(t) // 天然幂等
			return nil
		}
		if err := t.Cancel(s.now().UTC()); err != nil {
			return err
		}
		out = cloneTrain(t)
		return saveIdem(st.idem, requestID, "cancel", t.ID, fp, toWire(t))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Release 放行列车。审批角色全部通过才放行；放行与取消互斥。
// 重复放行（含不同 requestID 的重复调用）返回首次放行的同一个 ReleaseResult，
// outbox 中始终只有一条事件——状态迁移与事件写入在同一事务内完成。
func (s *Service) Release(trainID string, expectedVersion int, requestID string) (*ReleaseResult, error) {
	fp := fingerprint(struct {
		TrainID string `json:"train_id"`
	}{trainID})

	var out *ReleaseResult
	err := s.store.Update(func(st *storedState) error {
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}

		// 已放行：任何重复调用都重放首次结果，绝不产生第二条 outbox。
		if t.State() == StateReleased {
			out = &ReleaseResult{
				TrainID:    t.ID,
				ReleasedAt: t.ReleasedAt(),
				EventID:    t.ReleaseEventID(),
				SnapshotID: t.SnapshotID(),
				Idempotent: true,
			}
			return nil
		}
		if replayed, raw, err := checkIdem(st.idem, requestID, "release", fp); err != nil {
			return err
		} else if replayed {
			var r ReleaseResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			out = &r
			return nil
		}
		if err := checkVersion(t, expectedVersion); err != nil {
			return err
		}

		result, event, err := t.Release(s.now().UTC())
		if err != nil {
			return err
		}
		st.outbox = append(st.outbox, *event)
		out = result
		return saveIdem(st.idem, requestID, "release", t.ID, fp, result)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 查询 ----

// GetTrain 返回列车当前状态（克隆，调用方可安全持有）。
func (s *Service) GetTrain(id string) (*Train, error) {
	var out *Train
	err := s.store.View(func(st *storedState) error {
		t, err := requireTrain(st, id)
		if err != nil {
			return err
		}
		out = cloneTrain(t)
		return nil
	})
	return out, err
}

// ListTrains 返回全部列车 ID 与列车（按 ID 排序）。
func (s *Service) ListTrains() ([]*Train, error) {
	var out []*Train
	err := s.store.View(func(st *storedState) error {
		ids := make([]string, 0, len(st.trains))
		for id := range st.trains {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out = make([]*Train, 0, len(ids))
		for _, id := range ids {
			out = append(out, cloneTrain(st.trains[id]))
		}
		return nil
	})
	return out, err
}

// GetVersion 查询单个已登记组件版本。
func (s *Service) GetVersion(component string, v Version) (*ComponentVersion, error) {
	var out *ComponentVersion
	err := s.store.View(func(st *storedState) error {
		if byComp, ok := st.versions[component]; ok {
			if rec, ok := byComp[v.String()]; ok {
				out = &rec
				return nil
			}
		}
		return fmt.Errorf("%w: component %s version %s", ErrNotFound, component, v)
	})
	return out, err
}

// ListVersions 列出某组件（或全部组件，component 为空时）已登记版本。
func (s *Service) ListVersions(component string) ([]ComponentVersion, error) {
	var out []ComponentVersion
	err := s.store.View(func(st *storedState) error {
		comps := []string{}
		if component != "" {
			if _, ok := st.versions[component]; !ok {
				return fmt.Errorf("%w: component %s", ErrNotFound, component)
			}
			comps = []string{component}
		} else {
			for c := range st.versions {
				comps = append(comps, c)
			}
			sort.Strings(comps)
		}
		for _, c := range comps {
			keys := make([]string, 0, len(st.versions[c]))
			for k := range st.versions[c] {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				out = append(out, st.versions[c][k])
			}
		}
		return nil
	})
	return out, err
}

// PendingOutbox 返回尚未派发的发布事件（一列车至多一条）。
func (s *Service) PendingOutbox() ([]OutboxEvent, error) {
	var out []OutboxEvent
	err := s.store.View(func(st *storedState) error {
		for _, e := range st.outbox {
			if !e.Dispatched {
				out = append(out, e)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	})
	return out, err
}

// MarkOutboxDispatched 标记 outbox 事件已派发（供投递器使用）。
func (s *Service) MarkOutboxDispatched(eventID string) error {
	return s.store.Update(func(st *storedState) error {
		for i := range st.outbox {
			if st.outbox[i].ID == eventID {
				st.outbox[i].Dispatched = true
				return nil
			}
		}
		return fmt.Errorf("%w: outbox event %s", ErrNotFound, eventID)
	})
}
