package releasetrain

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	return NewService(store)
}

func v(s string) Version {
	parsed, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return parsed
}

// registerStack 登记一组相互兼容的组件版本：
// order 2.1.0（无依赖）、payment 1.2.0（需要 order >=2.0.0）、
// gateway 3.0.0（需要 payment =1.2.0 且 order >=2.0.0）。
func registerStack(t *testing.T, svc *Service) {
	t.Helper()
	mustRegister(t, svc, RegisterVersionInput{Component: "order", Version: v("2.1.0")})
	mustRegister(t, svc, RegisterVersionInput{Component: "payment", Version: v("1.2.0"), Constraints: []Constraint{
		{Component: "order", Op: OpGe, Version: v("2.0.0")},
	}})
	mustRegister(t, svc, RegisterVersionInput{Component: "gateway", Version: v("3.0.0"), Constraints: []Constraint{
		{Component: "order", Op: OpGe, Version: v("2.0.0")},
		{Component: "payment", Op: OpEq, Version: v("1.2.0")},
	}})
	// 一个不兼容版本，供失败场景使用。
	mustRegister(t, svc, RegisterVersionInput{Component: "payment", Version: v("1.0.0"), Constraints: []Constraint{
		{Component: "order", Op: OpGe, Version: v("3.0.0")},
	}})
}

func mustRegister(t *testing.T, svc *Service, in RegisterVersionInput) *ComponentVersion {
	t.Helper()
	rec, err := svc.RegisterVersion(in)
	if err != nil {
		t.Fatalf("register %s %s: %v", in.Component, in.Version, err)
	}
	return rec
}

func defaultPolicy(t *testing.T, svc *Service) {
	t.Helper()
	err := svc.PutPolicy(
		[]ApprovalRule{{Role: "qa", Need: 1}, {Role: "manager", Need: 1}},
		[]Approver{{Person: "alice", Role: "qa"}, {Person: "bob", Role: "manager"}},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func buildCompatibleTrain(t *testing.T, svc *Service) *Train {
	t.Helper()
	tr, err := svc.CreateTrain("2026-Q1", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		ver  string
	}{
		{"order", "2.1.0"}, {"payment", "1.2.0"}, {"gateway", "3.0.0"},
	} {
		tr, err = svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: c.name, Version: v(c.ver)})
		if err != nil {
			t.Fatalf("set candidate %s: %v", c.name, err)
		}
	}
	return tr
}

func freezeAndApprove(t *testing.T, svc *Service, id string) {
	t.Helper()
	if _, err := svc.Freeze(id, 0, ""); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := svc.Approve(id, "alice", "qa", 0, ""); err != nil {
		t.Fatalf("approve qa: %v", err)
	}
	if _, err := svc.Approve(id, "bob", "manager", 0, ""); err != nil {
		t.Fatalf("approve manager: %v", err)
	}
}

// ---- 场景 ----

func TestFullLifecycleRelease(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)

	freezeAndApprove(t, svc, tr.ID)

	res, err := svc.Release(tr.ID, 0, "")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.EventID == "" || res.SnapshotID == "" {
		t.Fatal("release result missing event/snapshot id")
	}
	got, _ := svc.GetTrain(tr.ID)
	if got.State() != StateReleased {
		t.Fatalf("state = %s", got.State())
	}
	events, _ := svc.PendingOutbox()
	if len(events) != 1 || events[0].ID != res.EventID {
		t.Fatalf("outbox = %+v", events)
	}
}

func TestFreezeFailsOnUnsatisfiedConstraintAndIsAtomic(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr, _ := svc.CreateTrain("t", "")
	// order 2.1.0 与 payment 1.0.0（要求 order >=3.0.0）不兼容。
	if _, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "order", Version: v("2.1.0")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "payment", Version: v("1.0.0")}); err != nil {
		t.Fatal(err)
	}
	verBefore := tr.Version + 2 // 两次 set 后
	cur, _ := svc.GetTrain(tr.ID)
	if cur.Version != verBefore {
		t.Fatalf("setup version = %d, want %d", cur.Version, verBefore)
	}

	_, err := svc.Freeze(tr.ID, 0, "")
	var depErr *DependencyError
	if !errors.As(err, &depErr) {
		t.Fatalf("want DependencyError, got %v", err)
	}
	if len(depErr.Problems) == 0 {
		t.Fatal("dependency error without problems")
	}
	// 原子性：冻结失败后仍是 editing，版本号不变，无快照、无策略快照。
	after, _ := svc.GetTrain(tr.ID)
	if after.State() != StateEditing {
		t.Fatalf("state after failed freeze = %s", after.State())
	}
	if after.Version != cur.Version {
		t.Fatalf("version changed on failed freeze: %d -> %d", cur.Version, after.Version)
	}
	if after.SnapshotID() != "" || after.Policy() != nil {
		t.Fatal("failed freeze left partial snapshot/policy")
	}
	// 修好候选后可以重新冻结成功。
	if _, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "payment", Version: v("1.2.0")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatalf("freeze after fix: %v", err)
	}
}

func TestFreezeFailsOnCycle(t *testing.T) {
	svc := newTestService(t)
	mustRegister(t, svc, RegisterVersionInput{Component: "a", Version: v("1.0.0"), Constraints: []Constraint{
		{Component: "b", Op: OpEq, Version: v("1.0.0")},
	}})
	mustRegister(t, svc, RegisterVersionInput{Component: "b", Version: v("1.0.0"), Constraints: []Constraint{
		{Component: "a", Op: OpEq, Version: v("1.0.0")},
	}})
	defaultPolicy(t, svc)

	tr, _ := svc.CreateTrain("cyc", "")
	if _, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "a", Version: v("1.0.0")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "b", Version: v("1.0.0")}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Freeze(tr.ID, 0, "")
	var depErr *DependencyError
	if !errors.As(err, &depErr) {
		t.Fatalf("want dependency cycle error, got %v", err)
	}
}

func TestFrozenSnapshotIsImmutable(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 冻结后不允许替换/移除候选。
	_, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "order", Version: v("2.1.0")})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("want state conflict, got %v", err)
	}
	_, err = svc.RemoveCandidate(tr.ID, "order", 0, "")
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("want state conflict, got %v", err)
	}
	// 不允许重复冻结。
	_, err = svc.Freeze(tr.ID, 0, "")
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("want state conflict on refreeze, got %v", err)
	}
}

func TestApprovalEligibilityAndIdempotency(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}

	// 无资格人员/角色被拒绝。
	if _, err := svc.Approve(tr.ID, "mallory", "qa", 0, ""); !errors.Is(err, ErrApproval) {
		t.Fatalf("want approval error, got %v", err)
	}
	if _, err := svc.Approve(tr.ID, "alice", "manager", 0, ""); !errors.Is(err, ErrApproval) {
		t.Fatalf("alice is qa not manager: %v", err)
	}

	// alice 以 qa 审批，重复审批幂等：审批记录仍只有一条，版本号不再增长。
	after1, _ := svc.Approve(tr.ID, "alice", "qa", 0, "")
	after2, err := svc.Approve(tr.ID, "alice", "qa", 0, "")
	if err != nil {
		t.Fatalf("duplicate approval should be idempotent: %v", err)
	}
	if after2.Version != after1.Version {
		t.Fatalf("duplicate approval bumped version: %d -> %d", after1.Version, after2.Version)
	}
	if len(after2.Approvals()) != 1 {
		t.Fatalf("approvals = %d, want 1", len(after2.Approvals()))
	}

	// 仅 qa 通过、manager 未通过时不能放行。
	_, err = svc.Release(tr.ID, 0, "")
	if !errors.Is(err, ErrApproval) {
		t.Fatalf("release before all roles: %v", err)
	}

	if _, err := svc.Approve(tr.ID, "bob", "manager", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Release(tr.ID, 0, ""); err != nil {
		t.Fatalf("release after all approvals: %v", err)
	}

	// 终态后再审批/冻结都被拒。
	if _, err := svc.Approve(tr.ID, "alice", "qa", 0, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("approve after release: %v", err)
	}
}

func TestPolicySnapshotFrozenAtFreeze(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}

	// 冻结后修改当前策略：换掉审批人、删掉 qa 规则，都不能影响本列车。
	err := svc.PutPolicy(
		[]ApprovalRule{{Role: "director", Need: 1}},
		[]Approver{{Person: "carol", Role: "director"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 旧快照里的 alice/qa、bob/manager 仍然有效。
	if _, err := svc.Approve(tr.ID, "alice", "qa", 0, ""); err != nil {
		t.Fatalf("frozen policy should still apply: %v", err)
	}
	if _, err := svc.Approve(tr.ID, "bob", "manager", 0, ""); err != nil {
		t.Fatalf("frozen policy should still apply: %v", err)
	}
	// 新策略里的 carol 对本列车无资格。
	if _, err := svc.Approve(tr.ID, "carol", "director", 0, ""); !errors.Is(err, ErrApproval) {
		t.Fatalf("new policy must not affect frozen train: %v", err)
	}
	if _, err := svc.Release(tr.ID, 0, ""); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestReleaseAndCancelAreMutuallyExclusive(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)

	// 先审批齐 -> 放行成功 -> 取消必须失败。
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)
	if _, err := svc.Release(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(tr.ID, 0, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("cancel after release: %v", err)
	}
	events, _ := svc.PendingOutbox()
	if len(events) != 1 {
		t.Fatalf("outbox should still have exactly 1 event, got %d", len(events))
	}

	// 另一列车：冻结后取消 -> 放行必须失败。
	tr2 := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr2.ID)
	if _, err := svc.Cancel(tr2.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Release(tr2.ID, 0, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("release after cancel: %v", err)
	}
	if got, _ := svc.GetTrain(tr2.ID); got.State() != StateCancelled {
		t.Fatalf("state = %s", got.State())
	}
	events2, _ := svc.PendingOutbox()
	if len(events2) != 1 {
		t.Fatalf("cancelled train must not emit events, outbox len = %d", len(events2))
	}
}

func TestReleaseIsIdempotentWithSingleOutboxEvent(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)

	first, err := svc.Release(tr.ID, 0, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	// 用不同 requestID、不带 requestID 重复放行，都返回同一结果。
	for i, rid := range []string{"req-2", "", "req-1"} {
		res, err := svc.Release(tr.ID, 0, rid)
		if err != nil {
			t.Fatalf("repeat release %d: %v", i, err)
		}
		if res.EventID != first.EventID || res.ReleasedAt != first.ReleasedAt {
			t.Fatalf("repeat %d returned different result: %+v vs %+v", i, res, first)
		}
	}
	events, _ := svc.PendingOutbox()
	if len(events) != 1 {
		t.Fatalf("outbox len = %d, want exactly 1", len(events))
	}
}

func TestOptimisticVersionConflict(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	stale := tr.Version // 当前版本

	// 第一次替换成功，版本增长。
	updated, err := svc.SetCandidate(SetCandidateInput{
		TrainID: tr.ID, Component: "order", Version: v("2.1.0"), ExpectedVersion: stale,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 拿着旧版本号再改，必须版本冲突。
	_, err = svc.SetCandidate(SetCandidateInput{
		TrainID: tr.ID, Component: "order", Version: v("2.1.0"), ExpectedVersion: stale,
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want version conflict, got %v", err)
	}
	// 用新版本号则成功。冻结也受版本条件保护。
	if _, err := svc.Freeze(tr.ID, updated.Version, ""); err != nil {
		t.Fatalf("freeze with current version: %v", err)
	}
	_, err = svc.Cancel(tr.ID, updated.Version, "")
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("cancel with stale version: %v", err)
	}
}

func TestConcurrentReleasesOnlyOneWins(t *testing.T) {
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(tr.ID, "alice", "qa", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(tr.ID, "bob", "manager", 0, ""); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.Release(tr.ID, 0, fmt.Sprintf("rel-%d", i))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent release error: %v", err)
		}
	}
	events, _ := svc.PendingOutbox()
	if len(events) != 1 {
		t.Fatalf("after %d concurrent releases outbox len = %d, want 1", n, len(events))
	}
}

func TestConcurrentCancelVsRelease(t *testing.T) {
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)

	var wg sync.WaitGroup
	wg.Add(2)
	var releaseErr, cancelErr error
	go func() { defer wg.Done(); _, releaseErr = svc.Release(tr.ID, 0, "") }()
	go func() { defer wg.Done(); _, cancelErr = svc.Cancel(tr.ID, 0, "") }()
	wg.Wait()

	got, _ := svc.GetTrain(tr.ID)
	switch got.State() {
	case StateReleased:
		if cancelErr == nil || !errors.Is(cancelErr, ErrStateConflict) {
			t.Fatalf("release won, cancel should conflict, got %v", cancelErr)
		}
		events, _ := svc.PendingOutbox()
		if len(events) != 1 {
			t.Fatalf("released outbox len = %d", len(events))
		}
	case StateCancelled:
		if releaseErr == nil || !errors.Is(releaseErr, ErrStateConflict) {
			t.Fatalf("cancel won, release should conflict, got %v", releaseErr)
		}
		events, _ := svc.PendingOutbox()
		if len(events) != 0 {
			t.Fatalf("cancelled train emitted %d events", len(events))
		}
	default:
		t.Fatalf("terminal state = %s", got.State())
	}
}

func TestConcurrentEditsOptimisticNoLostUpdate(t *testing.T) {
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr, _ := svc.CreateTrain("edit", "")
	cur, _ := svc.GetTrain(tr.ID)
	ver := cur.Version

	// 两个 goroutine 都基于同一版本做 set，至多一个成功，另一个必须版本冲突。
	var wg sync.WaitGroup
	wg.Add(2)
	res := make(chan error, 2)
	go func() {
		defer wg.Done()
		_, e := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "order", Version: v("2.1.0"), ExpectedVersion: ver})
		res <- e
	}()
	go func() {
		defer wg.Done()
		_, e := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "payment", Version: v("1.2.0"), ExpectedVersion: ver})
		res <- e
	}()
	wg.Wait()
	close(res)
	conflicts := 0
	successes := 0
	for e := range res {
		switch {
		case errors.Is(e, ErrVersionConflict):
			conflicts++
		case e == nil:
			successes++
		default:
			t.Fatalf("unexpected error %v", e)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("want 1 success + 1 conflict, got %d success %d conflict", successes, conflicts)
	}
}

func TestRegisterVersionImmutable(t *testing.T) {
	svc := newTestService(t)
	in := RegisterVersionInput{Component: "order", Version: v("1.0.0")}
	mustRegister(t, svc, in)
	// 完全重复登记幂等。
	if _, err := svc.RegisterVersion(in); err != nil {
		t.Fatalf("identical re-register should be idempotent: %v", err)
	}
	// 同一版本声明不同约束 => 幂等冲突（版本不可变）。
	bad := in
	bad.Constraints = []Constraint{{Component: "payment", Op: OpGe, Version: v("1.0.0")}}
	if _, err := svc.RegisterVersion(bad); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
}

func TestIdempotencyKeyReplay(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)

	// 同一 requestID + 同内容：返回缓存结果。
	t1, err := svc.CreateTrain("same", "create-1")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := svc.CreateTrain("same", "create-1")
	if err != nil {
		t.Fatal(err)
	}
	if t1.ID != t2.ID {
		t.Fatalf("idempotent replay created a second train: %s vs %s", t1.ID, t2.ID)
	}
	if n, _ := svc.ListTrains(); len(n) != 1 {
		t.Fatalf("trains len = %d, want 1", len(n))
	}

	// 同一 requestID 不同内容 => 幂等冲突。
	if _, err := svc.CreateTrain("different", "create-1"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
}

func TestCancelIdempotentAndEditingCancellable(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)

	// Editing 状态可直接取消。
	tr := buildCompatibleTrain(t, svc)
	c1, err := svc.Cancel(tr.ID, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if c1.State() != StateCancelled {
		t.Fatalf("state = %s", c1.State())
	}
	// 重复取消幂等，不改变状态；此后冻结被拒。
	c2, err := svc.Cancel(tr.ID, 0, "")
	if err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	if c2.State() != StateCancelled {
		t.Fatalf("state = %s", c2.State())
	}
	if _, err := svc.Freeze(tr.ID, 0, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("freeze after cancel: %v", err)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)
	res, err := svc.Release(tr.ID, 0, "")
	if err != nil {
		t.Fatal(err)
	}

	// 重新打开：状态、快照、策略快照、审批、outbox 全部恢复。
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(store2)
	got, err := svc2.GetTrain(tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State() != StateReleased || got.SnapshotID() != res.SnapshotID {
		t.Fatalf("restored train = %s snap=%s", got.State(), got.SnapshotID())
	}
	if len(got.Snapshot()) != 3 {
		t.Fatalf("snapshot = %v", got.Snapshot())
	}
	if got.Policy() == nil || len(got.Policy().Rules) != 2 || len(got.Approvals()) != 2 {
		t.Fatalf("policy/approvals not restored: %+v %+v", got.Policy(), got.Approvals())
	}
	if got.ReleaseEventID() != res.EventID {
		t.Fatalf("event id = %s, want %s", got.ReleaseEventID(), res.EventID)
	}
	events, _ := svc2.PendingOutbox()
	if len(events) != 1 || events[0].ID != res.EventID {
		t.Fatalf("outbox restored = %+v", events)
	}
	// 重复放行依旧幂等，落盘文件中 outbox 仍只有一条。
	again, err := svc2.Release(tr.ID, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.EventID != res.EventID {
		t.Fatal("replay after restart produced new event")
	}
	events2, _ := svc2.PendingOutbox()
	if len(events2) != 1 {
		t.Fatalf("outbox len after restart replay = %d", len(events2))
	}

	// 版本登记也持久化，且不可变约束仍然生效。
	_, err = svc2.RegisterVersion(RegisterVersionInput{Component: "order", Version: v("2.1.0"), Constraints: []Constraint{
		{Component: "payment", Op: OpEq, Version: v("9.9.9")},
	}})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("immutability after restart: %v", err)
	}
}

func TestQueries(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	vers, err := svc.ListVersions("payment")
	if err != nil {
		t.Fatal(err)
	}
	if len(vers) != 2 {
		t.Fatalf("payment versions = %d", len(vers))
	}
	all, err := svc.ListVersions("")
	if err != nil || len(all) != 4 {
		t.Fatalf("all versions = %d, err=%v", len(all), err)
	}
	if _, err := svc.GetVersion("payment", v("1.2.0")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetTrain("train_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestSetCandidateRejectsUnregistered(t *testing.T) {
	svc := newTestService(t)
	defaultPolicy(t, svc)
	tr, _ := svc.CreateTrain("t", "")
	_, err := svc.SetCandidate(SetCandidateInput{TrainID: tr.ID, Component: "ghost", Version: v("1.0.0")})
	if !errors.Is(err, ErrDependency) {
		t.Fatalf("want dependency error, got %v", err)
	}
}
