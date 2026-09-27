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
}

func newStoredState() *storedState {
	return &storedState{
		versions: map[string]map[string]ComponentVersion{},
		trains:   map[string]*Train{},
		idem:     map[string]idemRecord{},
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
	return c
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
}

func fromState(s *storedState) stateFile {
	trains := make(map[string]*trainWire, len(s.trains))
	for id, t := range s.trains {
		trains[id] = toWire(t)
	}
	return stateFile{
		Versions:    s.versions,
		Trains:      trains,
		Outbox:      s.outbox,
		Idempotency: s.idem,
		Policy:      s.policy,
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
	s.outbox = f.Outbox
	for id, w := range f.Trains {
		s.trains[id] = fromWire(w)
	}
	return s
}
