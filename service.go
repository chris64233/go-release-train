package releasetrain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Service 是发布列车的应用服务。所有方法并发安全：状态变更在单一存储锁内
// 校验 + 变更 + 原子落盘，配合修订号（乐观并发）避免并发丢失状态。
type Service struct {
	store *store
	now   func() time.Time
}

// NewService 打开一个以 path 为持久化文件的服务；":memory:" 或空串表示纯内存。
func NewService(path string) (*Service, error) {
	st, err := newStore(path)
	if err != nil {
		return nil, err
	}
	return &Service{store: st, now: time.Now}, nil
}

// SetClock 注入时钟，仅供测试使用。
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// ---------------------------------------------------------------------------
// 版本登记
// ---------------------------------------------------------------------------

// RegisterComponent 登记一个组件；重复登记同名组件是幂等的。
func (s *Service) RegisterComponent(name string) (Component, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Component{}, fail(KindValidation, "registerComponent", "component name is empty")
	}
	if strings.ContainsAny(name, "@\n\r") {
		return Component{}, fail(KindValidation, "registerComponent", "invalid component name %q", name)
	}
	err := s.store.mutate(func(d *storeData) error {
		if _, ok := d.Components[name]; ok {
			return nil
		}
		d.Components[name] = Component{Name: name, CreatedAt: s.now()}
		return nil
	})
	if err != nil {
		return Component{}, err
	}
	var c Component
	s.store.view(func(d *storeData) { c = d.Components[name] })
	return c, nil
}

// RegisterVersion 登记组件的一个不可变版本。
// 相同 component@version 再次登记：内容一致则幂等返回，内容不同则冲突（版本不可修改）。
func (s *Service) RegisterVersion(component, version string, deps []Dependency) (ComponentVersion, error) {
	component = strings.TrimSpace(component)
	ver, err := ParseVersion(version)
	if err != nil {
		return ComponentVersion{}, err
	}
	deps = append([]Dependency(nil), deps...)
	if err := validateDependencies(component, deps); err != nil {
		return ComponentVersion{}, err
	}
	key := component + "@" + ver.String()
	err = s.store.mutate(func(d *storeData) error {
		if _, ok := d.Components[component]; !ok {
			return fail(KindNotFound, "registerVersion", "component %q is not registered", component)
		}
		if existing, ok := d.Versions[key]; ok {
			if !depsEqual(existing.Dependencies, deps) {
				return fail(KindState, "registerVersion",
					"version %s is already published and immutable; existing dependencies differ", key)
			}
			return nil // 幂等重放
		}
		d.Versions[key] = ComponentVersion{
			Component:    component,
			Version:      ver.String(),
			Dependencies: deps,
			CreatedAt:    s.now(),
		}
		return nil
	})
	if err != nil {
		return ComponentVersion{}, err
	}
	var out ComponentVersion
	s.store.view(func(d *storeData) { out = d.Versions[key] })
	return out, nil
}

// GetComponent 查询组件。
func (s *Service) GetComponent(name string) (Component, error) {
	var c Component
	var ok bool
	s.store.view(func(d *storeData) { c, ok = d.Components[name] })
	if !ok {
		return Component{}, fail(KindNotFound, "getComponent", "component %q not found", name)
	}
	return c, nil
}

// ListComponents 按名字排序列出全部组件。
func (s *Service) ListComponents() []Component {
	var out []Component
	s.store.view(func(d *storeData) {
		for _, c := range d.Components {
			out = append(out, c)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListVersions 列出某组件的全部已登记版本，按语义化版本排序。
func (s *Service) ListVersions(component string) ([]ComponentVersion, error) {
	var out []ComponentVersion
	err := s.store.viewErr(func(d *storeData) error {
		if _, ok := d.Components[component]; !ok {
			return fail(KindNotFound, "listVersions", "component %q not found", component)
		}
		for _, v := range d.Versions {
			if v.Component == component {
				out = append(out, v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		vi, _ := ParseVersion(out[i].Version)
		vj, _ := ParseVersion(out[j].Version)
		return vi.Compare(vj) < 0
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// 审批策略
// ---------------------------------------------------------------------------

// SetPolicy 设置当前审批策略。注意：已冻结列车使用冻结瞬间的策略快照，不受影响。
func (s *Service) SetPolicy(rules []ApprovalRule) ([]ApprovalRule, error) {
	rules = append([]ApprovalRule(nil), rules...)
	if err := validatePolicy(rules); err != nil {
		return nil, err
	}
	if err := s.store.mutate(func(d *storeData) error {
		d.Policy = rules
		return nil
	}); err != nil {
		return nil, err
	}
	return append([]ApprovalRule(nil), rules...), nil
}

// GetPolicy 返回当前审批策略。
func (s *Service) GetPolicy() []ApprovalRule {
	var out []ApprovalRule
	s.store.view(func(d *storeData) { out = append(out, d.Policy...) })
	return out
}

// ---------------------------------------------------------------------------
// 列车编辑
// ---------------------------------------------------------------------------

// CreateTrain 创建一列 open 状态的列车。
func (s *Service) CreateTrain(id string, candidates []Candidate) (*Train, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fail(KindValidation, "createTrain", "train id is empty")
	}
	cands, err := normalizeCandidates(candidates)
	if err != nil {
		return nil, err
	}
	now := s.now()
	t := &Train{
		ID:         id,
		Revision:   1,
		State:      StateOpen,
		Candidates: cands,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	err = s.store.mutate(func(d *storeData) error {
		if _, ok := d.Trains[id]; ok {
			return fail(KindState, "createTrain", "train %q already exists", id)
		}
		for _, c := range cands {
			if _, ok := d.Versions[c.key()]; !ok {
				return fail(KindNotFound, "createTrain", "candidate %s is not registered", c.key())
			}
		}
		d.Trains[id] = cloneTrain(t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// SetCandidates 替换整份候选清单（仅 open 状态允许）。expectedRev 为乐观并发条件。
func (s *Service) SetCandidates(id string, candidates []Candidate, expectedRev int64) (*Train, error) {
	cands, err := normalizeCandidates(candidates)
	if err != nil {
		return nil, err
	}
	var out *Train
	err = s.store.mutate(func(d *storeData) error {
		t, ok := d.Trains[id]
		if !ok {
			return fail(KindNotFound, "setCandidates", "train %q not found", id)
		}
		if err := checkRevision(t, expectedRev, "setCandidates"); err != nil {
			return err
		}
		if t.State != StateOpen {
			return fail(KindState, "setCandidates", "train %q is %s, candidates are frozen", id, t.State)
		}
		for _, c := range cands {
			if _, ok := d.Versions[c.key()]; !ok {
				return fail(KindNotFound, "setCandidates", "candidate %s is not registered", c.key())
			}
		}
		t.Candidates = cands
		t.Revision++
		t.UpdatedAt = s.now()
		out = cloneTrain(t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetTrain 查询列车（返回深拷贝）。
func (s *Service) GetTrain(id string) (*Train, error) {
	var out *Train
	err := s.store.viewErr(func(d *storeData) error {
		t, ok := d.Trains[id]
		if !ok {
			return fail(KindNotFound, "getTrain", "train %q not found", id)
		}
		out = cloneTrain(t)
		return nil
	})
	return out, err
}

// ListTrains 按 ID 排序列出全部列车。
func (s *Service) ListTrains() []*Train {
	var out []*Train
	s.store.view(func(d *storeData) {
		for _, t := range d.Trains {
			out = append(out, cloneTrain(t))
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------------------------------------------------------------------------
// 冻结
// ---------------------------------------------------------------------------

// FreezeTrain 冻结列车：校验依赖图无环、且每个依赖约束都能在候选快照内满足。
// 任何一项不满足都会整体失败，列车保持 open，不产生任何部分结果。
// 冻结成功后保存当前审批策略的不可变快照。
func (s *Service) FreezeTrain(id string, expectedRev int64) (*Train, error) {
	var out *Train
	err := s.store.mutate(func(d *storeData) error {
		t, ok := d.Trains[id]
		if !ok {
			return fail(KindNotFound, "freezeTrain", "train %q not found", id)
		}
		if err := checkRevision(t, expectedRev, "freezeTrain"); err != nil {
			return err
		}
		if t.State == StateFrozen {
			return fail(KindState, "freezeTrain", "train %q is already frozen", id)
		}
		if t.State != StateOpen {
			return fail(KindState, "freezeTrain", "train %q is %s and cannot be frozen", id, t.State)
		}
		if len(t.Candidates) == 0 {
			return fail(KindValidation, "freezeTrain", "train %q has no candidates", id)
		}
		if err := validateSnapshot(t, d); err != nil {
			return err
		}
		// 防御性校验：策略本身必须可达成，否则冻结后永远无法放行。
		if err := validatePolicy(append([]ApprovalRule(nil), d.Policy...)); err != nil {
			return err
		}
		now := s.now()
		t.State = StateFrozen
		t.FrozenAt = now
		t.PolicySnapshot = append([]ApprovalRule(nil), d.Policy...)
		t.Revision++
		t.UpdatedAt = now
		out = cloneTrain(t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validateSnapshot 校验候选快照：版本存在、约束全部可在快照内满足、依赖图无环。
func validateSnapshot(t *Train, d *storeData) error {
	const op = "freezeTrain"
	chosen := t.candidateSet()

	resolved := make(map[string]ComponentVersion, len(t.Candidates))
	for _, c := range t.Candidates {
		v, ok := d.Versions[c.key()]
		if !ok {
			return fail(KindDependency, op, "candidate %s is not registered", c.key())
		}
		resolved[c.Component] = v
	}

	// 收集边并逐条校验约束。
	edges := make(map[string][]string)
	for _, c := range t.Candidates {
		v := resolved[c.Component]
		for _, dep := range v.Dependencies {
			target, ok := chosen[dep.Component]
			if !ok {
				return fail(KindDependency, op,
					"component %s requires %s %s, but %s is not in the candidate snapshot",
					c.Component, dep.Component, dep.Constraint, dep.Component)
			}
			targetVer, err := ParseVersion(target.Version)
			if err != nil {
				return fail(KindDependency, op, "candidate %s has unparsable version: %v", target.key(), err)
			}
			list, err := parseConstraintList(dep.Constraint)
			if err != nil {
				return fail(KindDependency, op, "component %s has invalid constraint on %s: %v",
					c.Component, dep.Component, err)
			}
			for _, con := range list {
				if !con.Satisfies(targetVer) {
					return fail(KindDependency, op,
						"component %s requires %s %s, but snapshot provides %s",
						c.Component, dep.Component, dep.Constraint, target.Version)
				}
			}
			edges[c.Component] = append(edges[c.Component], dep.Component)
		}
	}

	if cycle := findCycle(t.Candidates, edges); cycle != nil {
		return fail(KindDependency, op, "dependency graph has a cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// findCycle 用 DFS 三色标记查找环，返回形如 ["a","b","a"] 的环路径；无环返回 nil。
func findCycle(cands []Candidate, edges map[string][]string) []string {
	const white, gray, black = 0, 1, 2
	color := map[string]int{}
	var stack []string

	var dfs func(node string) []string
	dfs = func(node string) []string {
		color[node] = gray
		stack = append(stack, node)
		for _, next := range edges[node] {
			if color[next] == gray { // 回边 => 环
				path := append([]string{}, stack...)
				path = append(path, next)
				// 只保留从 next 第一次出现开始的段落
				for i, n := range path {
					if n == next {
						return path[i:]
					}
				}
				return path
			}
			if color[next] == white {
				if cyc := dfs(next); cyc != nil {
					return cyc
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = black
		return nil
	}

	// 固定遍历顺序，错误信息稳定。
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Component)
	}
	sort.Strings(names)
	for _, n := range names {
		sort.Strings(edges[n])
		if color[n] == white {
			if cyc := dfs(n); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 审批
// ---------------------------------------------------------------------------

// Approve 记录一次审批。资格（该人员是否属于策略快照中某角色）以冻结时保存的
// 策略快照为准；相同 (人员, 角色) 重复审批幂等返回原记录，不改变状态。
func (s *Service) Approve(trainID, person, role string, expectedRev int64) (*Train, Approval, error) {
	person = strings.TrimSpace(person)
	role = strings.TrimSpace(role)
	if person == "" || role == "" {
		return nil, Approval{}, fail(KindValidation, "approve", "person and role are required")
	}
	var out *Train
	var approval Approval
	err := s.store.mutate(func(d *storeData) error {
		t, ok := d.Trains[trainID]
		if !ok {
			return fail(KindNotFound, "approve", "train %q not found", trainID)
		}
		if t.State != StateFrozen {
			// 终态或未冻结：与修订号无关，直接报状态冲突。
			return fail(KindState, "approve", "train %q is %s, approvals are only accepted while frozen", trainID, t.State)
		}
		if err := checkRevision(t, expectedRev, "approve"); err != nil {
			return err
		}
		rule := findRule(t.PolicySnapshot, role)
		if rule == nil {
			return fail(KindApproval, "approve", "role %q is not required by the frozen policy of train %q", role, trainID)
		}
		if !contains(rule.Members, person) {
			return fail(KindApproval, "approve",
				"person %q is not a member of role %q in the frozen policy of train %q", person, role, trainID)
		}
		for _, a := range t.Approvals {
			if a.Person == person && a.Role == role {
				approval = a // 幂等：返回原审批
				out = cloneTrain(t)
				return nil
			}
		}
		approval = Approval{Person: person, Role: role, At: s.now()}
		t.Approvals = append(t.Approvals, approval)
		t.Revision++
		t.UpdatedAt = s.now()
		out = cloneTrain(t)
		return nil
	})
	if err != nil {
		return nil, Approval{}, err
	}
	return out, approval, nil
}

// RuleApprovalStatus 是单个审批角色的满足情况。
type RuleApprovalStatus struct {
	Role      string   `json:"role"`
	Threshold int      `json:"threshold"`
	Approved  []string `json:"approved"`
	Satisfied bool     `json:"satisfied"`
}

// ApprovalSummary 是整趟列车的审批满足情况。
type ApprovalSummary struct {
	Rules        []RuleApprovalStatus `json:"rules"`
	AllSatisfied bool                 `json:"all_satisfied"`
}

// ApprovalSummary 返回列车审批进展，规则口径取自冻结时的策略快照。
func (s *Service) ApprovalSummary(trainID string) (*ApprovalSummary, error) {
	var summary *ApprovalSummary
	err := s.store.viewErr(func(d *storeData) error {
		t, ok := d.Trains[trainID]
		if !ok {
			return fail(KindNotFound, "approvalSummary", "train %q not found", trainID)
		}
		summary = computeSummary(t)
		return nil
	})
	return summary, err
}

func computeSummary(t *Train) *ApprovalSummary {
	sum := &ApprovalSummary{AllSatisfied: true}
	for _, rule := range t.PolicySnapshot {
		st := RuleApprovalStatus{Role: rule.Role, Threshold: rule.Threshold}
		seen := map[string]bool{}
		for _, a := range t.Approvals {
			if a.Role == rule.Role && !seen[a.Person] {
				seen[a.Person] = true
				st.Approved = append(st.Approved, a.Person)
			}
		}
		sort.Strings(st.Approved)
		st.Satisfied = len(st.Approved) >= rule.Threshold
		if !st.Satisfied {
			sum.AllSatisfied = false
		}
		sum.Rules = append(sum.Rules, st)
	}
	return sum
}

// ---------------------------------------------------------------------------
// 取消
// ---------------------------------------------------------------------------

// CancelTrain 取消列车。open/frozen 可取消；released/cancelled 为终态，返回状态冲突。
func (s *Service) CancelTrain(id, reason string, expectedRev int64) (*Train, error) {
	var out *Train
	err := s.store.mutate(func(d *storeData) error {
		t, ok := d.Trains[id]
		if !ok {
			return fail(KindNotFound, "cancelTrain", "train %q not found", id)
		}
		switch t.State {
		case StateOpen, StateFrozen:
		default:
			// 终态：与修订号无关，直接报状态冲突。
			return fail(KindState, "cancelTrain", "train %q is already %s", id, t.State)
		}
		if err := checkRevision(t, expectedRev, "cancelTrain"); err != nil {
			return err
		}
		now := s.now()
		t.State = StateCancelled
		t.CancelledAt = now
		t.CancelReason = strings.TrimSpace(reason)
		t.Revision++
		t.UpdatedAt = now
		out = cloneTrain(t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 放行
// ---------------------------------------------------------------------------

// ReleaseTrain 放行冻结列车：必须所有审批角色满足阈值。成功后转为唯一终态
// released，并且 outbox 只产生一条事件；对已放行列车重复放行返回原结果。
func (s *Service) ReleaseTrain(id string, expectedRev int64) (*ReleaseResult, error) {
	var result *ReleaseResult
	err := s.store.mutate(func(d *storeData) error {
		t, ok := d.Trains[id]
		if !ok {
			return fail(KindNotFound, "releaseTrain", "train %q not found", id)
		}
		// 已放行：重复放行原样返回（幂等），不校验修订号。
		if t.State == StateReleased && t.ReleaseResult != nil {
			result = cloneReleaseResult(t.ReleaseResult)
			return nil
		}
		if t.State == StateCancelled {
			return fail(KindState, "releaseTrain", "train %q is cancelled and cannot be released", id)
		}
		if t.State != StateFrozen {
			return fail(KindState, "releaseTrain", "train %q is %s, only frozen trains can be released", id, t.State)
		}
		if err := checkRevision(t, expectedRev, "releaseTrain"); err != nil {
			return err
		}
		if sum := computeSummary(t); !sum.AllSatisfied {
			missing := []string{}
			for _, r := range sum.Rules {
				if !r.Satisfied {
					missing = append(missing, r.Role)
				}
			}
			return fail(KindApproval, "releaseTrain",
				"train %q cannot be released: approvals missing for roles: %s", id, strings.Join(missing, ", "))
		}

		now := s.now()
		payload, err := json.Marshal(map[string]any{
			"train_id":    t.ID,
			"revision":    t.Revision + 1,
			"snapshot":    t.Candidates,
			"released_at": now,
		})
		if err != nil {
			return err
		}
		d.OutboxSeq++
		event := OutboxEvent{
			ID:        d.OutboxSeq,
			Type:      "train.released",
			TrainID:   t.ID,
			Revision:  t.Revision + 1,
			Payload:   payload,
			CreatedAt: now,
		}
		d.Outbox = append(d.Outbox, event)

		t.State = StateReleased
		t.ReleasedAt = now
		t.Revision++
		t.UpdatedAt = now
		t.ReleaseResult = &ReleaseResult{
			TrainID:    t.ID,
			Revision:   t.Revision,
			OutboxID:   event.ID,
			ReleasedAt: now,
			Snapshot:   append([]Candidate(nil), t.Candidates...),
		}
		result = cloneReleaseResult(t.ReleaseResult)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// PendingOutbox 返回尚未发布的 outbox 事件，按 ID 升序。
func (s *Service) PendingOutbox() []OutboxEvent {
	var out []OutboxEvent
	s.store.view(func(d *storeData) {
		for _, e := range d.Outbox {
			if !e.Published {
				out = append(out, e)
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListOutbox 返回全部 outbox 事件。
func (s *Service) ListOutbox() []OutboxEvent {
	var out []OutboxEvent
	s.store.view(func(d *storeData) { out = append(out, d.Outbox...) })
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// MarkOutboxPublished 将事件标记为已发布（供投递方确认）。
func (s *Service) MarkOutboxPublished(id int64) error {
	return s.store.mutate(func(d *storeData) error {
		for i := range d.Outbox {
			if d.Outbox[i].ID == id {
				d.Outbox[i].Published = true
				return nil
			}
		}
		return fail(KindNotFound, "markOutboxPublished", "outbox event %d not found", id)
	})
}

// ---------------------------------------------------------------------------
// 幂等键（HTTP 层使用）
// ---------------------------------------------------------------------------

// Fingerprint 计算请求体指纹，用于识别同一幂等键下的不同请求。
func Fingerprint(method, path string, body []byte) string {
	sum := sha256.Sum256([]byte(method + "\n" + path + "\n" + string(body)))
	return hex.EncodeToString(sum[:])
}

// LoadIdempotency 读取幂等键记录；found 为 false 表示首次请求。
func (s *Service) LoadIdempotency(key string) (rec idemRecord, found bool) {
	s.store.view(func(d *storeData) {
		rec, found = d.Idempotency[key]
	})
	return rec, found
}

// SaveIdempotency 保存幂等键的响应。
func (s *Service) SaveIdempotency(key, fingerprint string, status int, response []byte) {
	_ = s.store.mutate(func(d *storeData) error {
		d.Idempotency[key] = idemRecord{
			Key:         key,
			Fingerprint: fingerprint,
			Status:      status,
			Response:    append([]byte(nil), response...),
			CreatedAt:   s.now(),
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func (s *store) viewErr(fn func(d *storeData) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(&s.data)
}

func checkRevision(t *Train, expected int64, op string) error {
	if expected > 0 && t.Revision != expected {
		return &Error{
			Kind:    KindVersion,
			Op:      op,
			Msg:     "revision mismatch: state has moved on, re-read the train and retry",
			Current: t.Revision,
			err:     ErrVersionConflict,
		}
	}
	return nil
}

func normalizeCandidates(in []Candidate) ([]Candidate, error) {
	if len(in) == 0 {
		return nil, fail(KindValidation, "normalizeCandidates", "candidate list is empty")
	}
	out := make([]Candidate, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c.Component = strings.TrimSpace(c.Component)
		c.Version = strings.TrimSpace(c.Version)
		if c.Component == "" || c.Version == "" {
			return nil, fail(KindValidation, "normalizeCandidates", "candidate component and version are required")
		}
		v, err := ParseVersion(c.Version)
		if err != nil {
			return nil, fail(KindValidation, "normalizeCandidates", "candidate %s: %v", c.Component, err)
		}
		c.Version = v.String()
		if seen[c.Component] {
			return nil, fail(KindValidation, "normalizeCandidates",
				"component %s appears more than once in candidate list", c.Component)
		}
		seen[c.Component] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	return out, nil
}

func validateDependencies(owner string, deps []Dependency) error {
	for _, dep := range deps {
		dep.Component = strings.TrimSpace(dep.Component)
		dep.Constraint = strings.TrimSpace(dep.Constraint)
		if dep.Component == "" || dep.Constraint == "" {
			return fail(KindValidation, "registerVersion", "dependency of %s must name component and constraint", owner)
		}
		if dep.Component == owner {
			return fail(KindValidation, "registerVersion", "component %s must not depend on itself", owner)
		}
		if _, err := parseConstraintList(dep.Constraint); err != nil {
			return fail(KindValidation, "registerVersion", "component %s has invalid constraint on %s: %v",
				owner, dep.Component, err)
		}
	}
	return nil
}

// parseConstraintList 解析逗号分隔的 AND 约束列表，如 ">=1.0.0,<2.0.0"。
func parseConstraintList(s string) ([]Constraint, error) {
	var out []Constraint
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		c, err := ParseConstraint(part)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fail(KindValidation, "parseConstraintList", "empty constraint")
	}
	return out, nil
}

func validatePolicy(rules []ApprovalRule) error {
	seen := map[string]bool{}
	for _, r := range rules {
		r.Role = strings.TrimSpace(r.Role)
		if r.Role == "" {
			return fail(KindValidation, "setPolicy", "approval rule has empty role")
		}
		if seen[r.Role] {
			return fail(KindValidation, "setPolicy", "duplicate approval role %q", r.Role)
		}
		seen[r.Role] = true
		if r.Threshold < 1 {
			return fail(KindValidation, "setPolicy", "role %q threshold must be >= 1", r.Role)
		}
		members := map[string]bool{}
		for _, m := range r.Members {
			m = strings.TrimSpace(m)
			if m == "" {
				return fail(KindValidation, "setPolicy", "role %q has an empty member", r.Role)
			}
			if members[m] {
				return fail(KindValidation, "setPolicy", "role %q lists member %q more than once", r.Role, m)
			}
			members[m] = true
		}
		if r.Threshold > len(r.Members) {
			return fail(KindValidation, "setPolicy",
				"role %q threshold %d exceeds its member count %d", r.Role, r.Threshold, len(r.Members))
		}
	}
	return nil
}

func findRule(rules []ApprovalRule, role string) *ApprovalRule {
	for i := range rules {
		if rules[i].Role == role {
			return &rules[i]
		}
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func depsEqual(a, b []Dependency) bool {
	if len(a) != len(b) {
		return false
	}
	ca := append([]Dependency(nil), a...)
	cb := append([]Dependency(nil), b...)
	sort.Slice(ca, func(i, j int) bool { return ca[i].Component < ca[j].Component })
	sort.Slice(cb, func(i, j int) bool { return cb[i].Component < cb[j].Component })
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

func cloneTrain(t *Train) *Train {
	if t == nil {
		return nil
	}
	c := *t
	c.Candidates = append([]Candidate(nil), t.Candidates...)
	c.PolicySnapshot = append([]ApprovalRule(nil), t.PolicySnapshot...)
	c.Approvals = append([]Approval(nil), t.Approvals...)
	if t.ReleaseResult != nil {
		c.ReleaseResult = cloneReleaseResult(t.ReleaseResult)
	}
	return &c
}

func cloneReleaseResult(r *ReleaseResult) *ReleaseResult {
	if r == nil {
		return nil
	}
	c := *r
	c.Snapshot = append([]Candidate(nil), r.Snapshot...)
	return &c
}
