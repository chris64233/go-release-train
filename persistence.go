package releasetrain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// idemRecord 记录一次幂等请求的指纹与成功响应。
type idemRecord struct {
	Kind        string          `json:"kind"`
	TrainID     string          `json:"train_id,omitempty"`
	Fingerprint string          `json:"fingerprint"`
	Result      json.RawMessage `json:"result"`
}

// storedState 是持久化的全部状态。Service 的所有修改都在 Store.Update
// 给出的副本上进行：业务函数返回错误时副本被丢弃，绝不会留下部分结果；
// 成功时整份状态原子写盘（临时文件 + rename）。
type storedState struct {
	// versions: component -> "major.minor.patch" -> 已登记版本（不可变）
	versions map[string]map[string]ComponentVersion
	trains   map[string]*Train
	outbox   []OutboxEvent
	idem     map[string]idemRecord
	// policy 是当前（可变）的审批策略；冻结时复制进列车成为策略快照。
	policy *PolicySnapshot

	// envPolicy 是环境顺序与各环境审批门槛的当前（可变）定义；
	// 创建晋级活动时复制成活动内快照。
	envPolicy *EnvironmentPolicy
	// promotions 是全部环境晋级活动（按列车）。
	promotions map[string]*Promotion
	// live 是现网实际部署版本：environment -> component -> version。
	// 部署成功/回退在同一事务内更新，供查询展示与新尝试继承判断。
	live map[string]map[string]Version
}

func newStoredState() *storedState {
	return &storedState{
		versions:   map[string]map[string]ComponentVersion{},
		trains:     map[string]*Train{},
		idem:       map[string]idemRecord{},
		promotions: map[string]*Promotion{},
		live:       map[string]map[string]Version{},
	}
}

func (s *storedState) clone() *storedState {
	c := newStoredState()
	for comp, m := range s.versions {
		cp := make(map[string]ComponentVersion, len(m))
		for k, v := range m {
			cp[k] = cloneVersionRecord(v)
		}
		c.versions[comp] = cp
	}
	for id, t := range s.trains {
		c.trains[id] = cloneTrain(t)
	}
	c.outbox = append(c.outbox, s.outbox...)
	for k, v := range s.idem {
		c.idem[k] = v
	}
	if s.policy != nil {
		p := *s.policy
		p.Rules = append([]ApprovalRule(nil), s.policy.Rules...)
		p.Approvers = append([]Approver(nil), s.policy.Approvers...)
		c.policy = &p
	}
	if s.envPolicy != nil {
		c.envPolicy = cloneEnvironmentPolicy(s.envPolicy)
	}
	for id, pr := range s.promotions {
		c.promotions[id] = clonePromotion(pr)
	}
	for env, m := range s.live {
		cp := make(map[string]Version, len(m))
		for k, v := range m {
			cp[k] = v
		}
		c.live[env] = cp
	}
	return c
}

func cloneEnvironmentPolicy(p *EnvironmentPolicy) *EnvironmentPolicy {
	c := &EnvironmentPolicy{Environments: make([]EnvPolicy, len(p.Environments))}
	for i, e := range p.Environments {
		c.Environments[i] = EnvPolicy{
			Name:      e.Name,
			Rules:     append([]ApprovalRule(nil), e.Rules...),
			Approvers: append([]Approver(nil), e.Approvers...),
		}
	}
	return c
}

func clonePromotion(p *Promotion) *Promotion {
	cp := *p
	cp.snapshot = cloneCandidates(p.snapshot)
	cp.environments = append([]string(nil), p.environments...)
	cp.envPolicies = map[string]EnvPolicy{}
	for k, v := range p.envPolicies {
		cp.envPolicies[k] = EnvPolicy{
			Name:      v.Name,
			Rules:     append([]ApprovalRule(nil), v.Rules...),
			Approvers: append([]Approver(nil), v.Approvers...),
		}
	}
	cp.attempts = make([]*Attempt, len(p.attempts))
	for i, a := range p.attempts {
		ca := *a
		ca.envs = make([]*EnvExecution, len(a.envs))
		for j, e := range a.envs {
			ce := *e
			ce.policy = EnvPolicy{
				Name:      e.policy.Name,
				Rules:     append([]ApprovalRule(nil), e.policy.Rules...),
				Approvers: append([]Approver(nil), e.policy.Approvers...),
			}
			ce.approvals = append([]EnvApproval(nil), e.approvals...)
			ce.beforeVersions = cloneVersionPtrs(e.beforeVersions)
			ce.results = append([]ComponentDeployResult(nil), e.results...)
			for k := range ce.results {
				if e.results[k].DeployedVersion != nil {
					v := *e.results[k].DeployedVersion
					ce.results[k].DeployedVersion = &v
				}
			}
			ce.rollback = make([]*RollbackUnit, len(e.rollback))
			for k, u := range e.rollback {
				cu := *u
				if u.ToVersion != nil {
					v := *u.ToVersion
					cu.ToVersion = &v
				}
				ce.rollback[k] = &cu
			}
			ca.envs[j] = &ce
		}
		cp.attempts[i] = &ca
	}
	return &cp
}

func cloneVersionRecord(v ComponentVersion) ComponentVersion {
	if v.Constraints != nil {
		v.Constraints = append([]Constraint(nil), v.Constraints...)
	}
	return v
}

func cloneTrain(t *Train) *Train {
	cp := *t
	cp.candidates = cloneCandidates(t.candidates)
	cp.frozenSnapshot = cloneCandidates(t.frozenSnapshot)
	if t.policy != nil {
		p := *t.policy
		if t.policy.Rules != nil {
			p.Rules = append([]ApprovalRule(nil), t.policy.Rules...)
		}
		if t.policy.Approvers != nil {
			p.Approvers = append([]Approver(nil), t.policy.Approvers...)
		}
		cp.policy = &p
	}
	if t.approvals != nil {
		cp.approvals = append([]Approval(nil), t.approvals...)
	}
	return &cp
}

// Store 是持久化抽象。Update 在排他锁内把 fn 作用于状态副本，
// fn 成功才提交（内存态替换 + 落盘），失败则回滚。
type Store interface {
	Update(fn func(*storedState) error) error
	View(fn func(*storedState) error) error
}

// FileStore 用单个 JSON 文件做持久化；path 为空时退化为纯内存存储（测试用）。
type FileStore struct {
	mu   sync.RWMutex
	path string
	now  func() time.Time
	cur  *storedState
}

// NewFileStore 打开（或创建）path 处的状态文件。path 为空则只在内存保存。
func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, now: time.Now, cur: newStoredState()}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		// 空库，稍后首次提交时创建
	case err != nil:
		return nil, err
	default:
		if len(data) > 0 {
			var f stateFile
			if err := json.Unmarshal(data, &f); err != nil {
				return nil, err
			}
			s.cur = f.toState()
		}
	}
	return s, nil
}

// Update 见 Store 接口约定。
func (s *FileStore) Update(fn func(*storedState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	work := s.cur.clone()
	if err := fn(work); err != nil {
		return err // 副本丢弃，无任何部分提交
	}
	if err := s.persist(work); err != nil {
		return err
	}
	s.cur = work
	return nil
}

// View 在锁内克隆状态后交给 fn 读取，fn 看到的是一致快照。
func (s *FileStore) View(fn func(*storedState) error) error {
	s.mu.RLock()
	snap := s.cur.clone()
	s.mu.RUnlock()
	return fn(snap)
}

func (s *FileStore) persist(st *storedState) error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(fromState(st), "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path) // 同目录原子替换
}

// ---- JSON 线格式（Train 有私有字段，经 trainWire 转换） ----

type trainWire struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	State          TrainState         `json:"state"`
	Version        int                `json:"version"`
	Candidates     map[string]Version `json:"candidates,omitempty"`
	FrozenSnapshot map[string]Version `json:"frozen_snapshot,omitempty"`
	SnapshotID     string             `json:"snapshot_id,omitempty"`
	FrozenAt       time.Time          `json:"frozen_at,omitempty"`
	Policy         *PolicySnapshot    `json:"policy,omitempty"`
	Approvals      []Approval         `json:"approvals,omitempty"`
	ReleasedAt     time.Time          `json:"released_at,omitempty"`
	CancelledAt    time.Time          `json:"cancelled_at,omitempty"`
	ReleaseEventID string             `json:"release_event_id,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

func toWire(t *Train) *trainWire {
	return &trainWire{
		ID:             t.ID,
		Name:           t.Name,
		State:          t.state,
		Version:        t.Version,
		Candidates:     t.candidates,
		FrozenSnapshot: t.frozenSnapshot,
		SnapshotID:     t.snapshotID,
		FrozenAt:       t.frozenAt,
		Policy:         t.policy,
		Approvals:      t.approvals,
		ReleasedAt:     t.releasedAt,
		CancelledAt:    t.cancelledAt,
		ReleaseEventID: t.releaseEventID,
		CreatedAt:      t.createdAt,
		UpdatedAt:      t.updatedAt,
	}
}

func fromWire(w *trainWire) *Train {
	return &Train{
		ID:             w.ID,
		Name:           w.Name,
		state:          w.State,
		Version:        w.Version,
		candidates:     w.Candidates,
		frozenSnapshot: w.FrozenSnapshot,
		snapshotID:     w.SnapshotID,
		frozenAt:       w.FrozenAt,
		policy:         w.Policy,
		approvals:      w.Approvals,
		releasedAt:     w.ReleasedAt,
		cancelledAt:    w.CancelledAt,
		releaseEventID: w.ReleaseEventID,
		createdAt:      w.CreatedAt,
		updatedAt:      w.UpdatedAt,
	}
}

type stateFile struct {
	Versions    map[string]map[string]ComponentVersion `json:"versions"`
	Trains      map[string]*trainWire                  `json:"trains"`
	Outbox      []OutboxEvent                          `json:"outbox"`
	Idempotency map[string]idemRecord                  `json:"idempotency"`
	Policy      *PolicySnapshot                        `json:"policy,omitempty"`
	EnvPolicy   *EnvironmentPolicy                     `json:"environment_policy,omitempty"`
	Promotions  map[string]*promotionWire              `json:"promotions"`
	Live        map[string]map[string]Version          `json:"live"`
}

func fromState(s *storedState) stateFile {
	trains := make(map[string]*trainWire, len(s.trains))
	for id, t := range s.trains {
		trains[id] = toWire(t)
	}
	promotions := make(map[string]*promotionWire, len(s.promotions))
	for id, p := range s.promotions {
		promotions[id] = promotionToWire(p)
	}
	return stateFile{
		Versions:    s.versions,
		Trains:      trains,
		Outbox:      s.outbox,
		Idempotency: s.idem,
		Policy:      s.policy,
		EnvPolicy:   s.envPolicy,
		Promotions:  promotions,
		Live:        s.live,
	}
}

func (f stateFile) toState() *storedState {
	s := newStoredState()
	if f.Versions != nil {
		s.versions = f.Versions
	}
	if f.Idempotency != nil {
		s.idem = f.Idempotency
	}
	s.policy = f.Policy
	s.envPolicy = f.EnvPolicy
	s.outbox = f.Outbox
	if f.Promotions != nil {
		s.promotions = map[string]*Promotion{}
		for id, w := range f.Promotions {
			s.promotions[id] = promotionFromWire(w)
		}
	}
	if f.Live != nil {
		s.live = f.Live
	}
	for id, w := range f.Trains {
		s.trains[id] = fromWire(w)
	}
	return s
}

// ---- Promotion 的 JSON 线格式（聚合含私有字段，经 promotionWire 转换） ----

type rollbackUnitWire struct {
	Component   string            `json:"component"`
	ToVersion   *Version          `json:"to_version,omitempty"`
	State       RollbackUnitState `json:"state"`
	LeaseNo     int               `json:"lease_no"`
	Worker      string            `json:"worker,omitempty"`
	LeasedAt    time.Time         `json:"leased_at,omitempty"`
	Attempts    int               `json:"attempts"`
	LastMessage string            `json:"last_message,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
	SucceededAt time.Time         `json:"succeeded_at,omitempty"`
}

type envExecutionWire struct {
	Name               string                  `json:"name"`
	Status             EnvStatus               `json:"status"`
	Policy             EnvPolicy               `json:"policy"`
	Approvals          []EnvApproval           `json:"approvals,omitempty"`
	BeforeVersions     map[string]*Version     `json:"before_versions,omitempty"`
	Results            []ComponentDeployResult `json:"results,omitempty"`
	LeaseNo            int                     `json:"lease_no"`
	LeaseWorker        string                  `json:"lease_worker,omitempty"`
	LeasedAt           time.Time               `json:"leased_at,omitempty"`
	PromotedAt         time.Time               `json:"promoted_at,omitempty"`
	FailedAt           time.Time               `json:"failed_at,omitempty"`
	EventID            string                  `json:"event_id,omitempty"`
	CarriedFromAttempt int                     `json:"carried_from_attempt,omitempty"`
	Rollback           []*rollbackUnitWire     `json:"rollback,omitempty"`
}

type attemptWire struct {
	No         int                 `json:"no"`
	Status     AttemptStatus       `json:"status"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt time.Time           `json:"finished_at,omitempty"`
	Envs       []*envExecutionWire `json:"envs"`
}

type promotionWire struct {
	ID           string               `json:"id"`
	TrainID      string               `json:"train_id"`
	State        PromotionState       `json:"state"`
	Version      int                  `json:"version"`
	Snapshot     map[string]Version   `json:"snapshot"`
	SnapshotID   string               `json:"snapshot_id"`
	Environments []string             `json:"environments"`
	EnvPolicies  map[string]EnvPolicy `json:"env_policies"`
	Attempts     []*attemptWire       `json:"attempts"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
	SucceededAt  time.Time            `json:"succeeded_at,omitempty"`
	CancelledAt  time.Time            `json:"cancelled_at,omitempty"`
}

func promotionToWire(p *Promotion) *promotionWire {
	w := &promotionWire{
		ID:           p.ID,
		TrainID:      p.TrainID,
		State:        p.state,
		Version:      p.Version,
		Snapshot:     p.snapshot,
		SnapshotID:   p.snapshotID,
		Environments: p.environments,
		EnvPolicies:  p.envPolicies,
		CreatedAt:    p.createdAt,
		UpdatedAt:    p.updatedAt,
		SucceededAt:  p.succeededAt,
		CancelledAt:  p.cancelledAt,
	}
	for _, a := range p.attempts {
		aw := &attemptWire{No: a.No, Status: a.Status, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt}
		for _, e := range a.envs {
			ew := &envExecutionWire{
				Name:               e.Name,
				Status:             e.Status,
				Policy:             e.policy,
				Approvals:          e.approvals,
				BeforeVersions:     e.beforeVersions,
				Results:            e.results,
				LeaseNo:            e.leaseNo,
				LeaseWorker:        e.leaseWorker,
				LeasedAt:           e.leasedAt,
				PromotedAt:         e.promotedAt,
				FailedAt:           e.failedAt,
				EventID:            e.eventID,
				CarriedFromAttempt: e.carriedFromAttempt,
			}
			for _, u := range e.rollback {
				ew.Rollback = append(ew.Rollback, &rollbackUnitWire{
					Component:   u.Component,
					ToVersion:   u.ToVersion,
					State:       u.State,
					LeaseNo:     u.LeaseNo,
					Worker:      u.Worker,
					LeasedAt:    u.LeasedAt,
					Attempts:    u.Attempts,
					LastMessage: u.LastMessage,
					UpdatedAt:   u.UpdatedAt,
					SucceededAt: u.SucceededAt,
				})
			}
			aw.Envs = append(aw.Envs, ew)
		}
		w.Attempts = append(w.Attempts, aw)
	}
	return w
}

func promotionFromWire(w *promotionWire) *Promotion {
	p := &Promotion{
		ID:           w.ID,
		TrainID:      w.TrainID,
		state:        w.State,
		Version:      w.Version,
		snapshot:     w.Snapshot,
		snapshotID:   w.SnapshotID,
		environments: w.Environments,
		envPolicies:  w.EnvPolicies,
		createdAt:    w.CreatedAt,
		updatedAt:    w.UpdatedAt,
		succeededAt:  w.SucceededAt,
		cancelledAt:  w.CancelledAt,
	}
	for _, aw := range w.Attempts {
		a := &Attempt{No: aw.No, Status: aw.Status, StartedAt: aw.StartedAt, FinishedAt: aw.FinishedAt}
		for _, ew := range aw.Envs {
			e := &EnvExecution{
				Name:               ew.Name,
				Status:             ew.Status,
				policy:             ew.Policy,
				approvals:          ew.Approvals,
				beforeVersions:     ew.BeforeVersions,
				results:            ew.Results,
				leaseNo:            ew.LeaseNo,
				leaseWorker:        ew.LeaseWorker,
				leasedAt:           ew.LeasedAt,
				promotedAt:         ew.PromotedAt,
				failedAt:           ew.FailedAt,
				eventID:            ew.EventID,
				carriedFromAttempt: ew.CarriedFromAttempt,
			}
			for _, uw := range ew.Rollback {
				e.rollback = append(e.rollback, &RollbackUnit{
					Component:   uw.Component,
					ToVersion:   uw.ToVersion,
					State:       uw.State,
					LeaseNo:     uw.LeaseNo,
					Worker:      uw.Worker,
					LeasedAt:    uw.LeasedAt,
					Attempts:    uw.Attempts,
					LastMessage: uw.LastMessage,
					UpdatedAt:   uw.UpdatedAt,
					SucceededAt: uw.SucceededAt,
				})
			}
			a.envs = append(a.envs, e)
		}
		p.attempts = append(p.attempts, a)
	}
	return p
}
