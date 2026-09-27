package releasetrain

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// fixture 构造一组常用组件：
//
//	web   1.0.0 -> api ^1.0.0
//	api   1.0.0 -> lib >=1.0.0,<2.0.0
//	api   1.2.0 -> lib >=1.1.0,<2.0.0
//	lib   1.0.0
//	lib   1.1.0
//	loopy 1.0.0 -> loopb ^1.0.0 ; loopb 1.0.0 -> loopy ^1.0.0 （环）
type fixture struct {
	t   *testing.T
	svc *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	svc, err := NewService(":memory:")
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f := &fixture{t: t, svc: svc}
	f.mustRegister("web", "1.0.0", []Dependency{{"api", "^1.0.0"}})
	f.mustRegister("api", "1.0.0", []Dependency{{"lib", ">=1.0.0,<2.0.0"}})
	f.mustRegister("api", "1.2.0", []Dependency{{"lib", ">=1.1.0,<2.0.0"}})
	f.mustRegister("lib", "1.0.0", nil)
	f.mustRegister("lib", "1.1.0", nil)
	f.mustRegister("loopy", "1.0.0", []Dependency{{"loopb", "^1.0.0"}})
	f.mustRegister("loopb", "1.0.0", []Dependency{{"loopy", "^1.0.0"}})
	return f
}

func (f *fixture) mustRegister(component, version string, deps []Dependency) {
	f.t.Helper()
	if _, err := f.svc.RegisterComponent(component); err != nil {
		f.t.Fatalf("register component %s: %v", component, err)
	}
	if _, err := f.svc.RegisterVersion(component, version, deps); err != nil {
		f.t.Fatalf("register version %s@%s: %v", component, version, err)
	}
}

func (f *fixture) mustTrain(id string, cands ...Candidate) *Train {
	f.t.Helper()
	train, err := f.svc.CreateTrain(id, cands)
	if err != nil {
		f.t.Fatalf("create train %s: %v", id, err)
	}
	return train
}

func cands(pairs ...string) []Candidate {
	out := []Candidate{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Candidate{Component: pairs[i], Version: pairs[i+1]})
	}
	return out
}

func standardPolicy() []ApprovalRule {
	return []ApprovalRule{
		{Role: "qa", Members: []string{"alice", "erin"}, Threshold: 1},
		{Role: "manager", Members: []string{"bob"}, Threshold: 1},
	}
}

// ---------------------------------------------------------------------------
// 版本登记
// ---------------------------------------------------------------------------

func TestRegisterVersionImmutable(t *testing.T) {
	f := newFixture(t)

	// 相同内容重复登记 -> 幂等成功。
	if _, err := f.svc.RegisterVersion("lib", "1.0.0", nil); err != nil {
		t.Fatalf("idempotent re-register failed: %v", err)
	}

	// 不同依赖重复登记同一版本 -> 不可修改冲突。
	_, err := f.svc.RegisterVersion("lib", "1.0.0", []Dependency{{"api", "^1.0.0"}})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("expected state conflict, got %v", err)
	}

	// 未登记组件不允许登记版本。
	_, err = f.svc.RegisterVersion("ghost", "1.0.0", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}

	// 非法版本号。
	_, err = f.svc.RegisterVersion("lib", "not-a-version", nil)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestRegisterVersionRejectsSelfDependency(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.RegisterVersion("lib", "9.9.9", []Dependency{{"lib", "^1.0.0"}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation error for self dependency, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 列车编辑与冻结
// ---------------------------------------------------------------------------

func TestFreezeValidSnapshot(t *testing.T) {
	f := newFixture(t)
	train := f.mustTrain("T1", cands("web", "1.0.0", "api", "1.2.0", "lib", "1.1.0")...)
	if train.Revision != 1 {
		t.Fatalf("initial revision = %d, want 1", train.Revision)
	}

	if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	frozen, err := f.svc.FreezeTrain("T1", 1)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if frozen.State != StateFrozen || frozen.Revision != 2 {
		t.Fatalf("unexpected frozen train: %+v", frozen)
	}
	if len(frozen.PolicySnapshot) != 2 {
		t.Fatalf("policy snapshot not saved: %+v", frozen.PolicySnapshot)
	}
	if frozen.FrozenAt.IsZero() {
		t.Fatal("FrozenAt not set")
	}
}

func TestFreezeUnsatisfiedConstraint(t *testing.T) {
	f := newFixture(t)
	// api 1.2.0 需要 lib >=1.1.0，但快照只提供 lib 1.0.0。
	train := f.mustTrain("T1", cands("web", "1.0.0", "api", "1.2.0", "lib", "1.0.0")...)

	_, err := f.svc.FreezeTrain("T1", train.Revision)
	if !errors.Is(err, ErrDependencyConflict) {
		t.Fatalf("expected dependency conflict, got %v", err)
	}

	// 整体失败：列车必须仍是 open 且修订号未变，没有任何部分结果。
	after, _ := f.svc.GetTrain("T1")
	if after.State != StateOpen {
		t.Fatalf("state = %s, want open", after.State)
	}
	if after.Revision != train.Revision {
		t.Fatalf("revision changed on failed freeze: %d -> %d", train.Revision, after.Revision)
	}
	if after.PolicySnapshot != nil || !after.FrozenAt.IsZero() {
		t.Fatal("partial freeze result left behind")
	}
}

func TestFreezeMissingDependencyInSnapshot(t *testing.T) {
	f := newFixture(t)
	// web 依赖 api，但快照里没有 api。
	train := f.mustTrain("T1", cands("web", "1.0.0")...)
	_, err := f.svc.FreezeTrain("T1", train.Revision)
	if !errors.Is(err, ErrDependencyConflict) {
		t.Fatalf("expected dependency conflict, got %v", err)
	}
}

func TestFreezeCycle(t *testing.T) {
	f := newFixture(t)
	train := f.mustTrain("T1", cands("loopy", "1.0.0", "loopb", "1.0.0")...)
	_, err := f.svc.FreezeTrain("T1", train.Revision)
	if !errors.Is(err, ErrDependencyConflict) {
		t.Fatalf("expected dependency conflict for cycle, got %v", err)
	}
	if Kind(err) != KindDependency {
		t.Fatalf("error kind = %q, want dependency", Kind(err))
	}
}

func TestFreezeOnlyOnceAndNotWhenCancelled(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)

	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := f.svc.FreezeTrain("T1", train.Revision); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale revision freeze: expected version conflict, got %v", err)
	}
	if _, err := f.svc.FreezeTrain("T1", 2); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("second freeze: expected state conflict, got %v", err)
	}

	// 取消后不可冻结。
	t2 := f.mustTrain("T2", cands("lib", "1.0.0")...)
	if _, err := f.svc.CancelTrain("T2", "nvm", t2.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FreezeTrain("T2", t2.Revision+1); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("freeze after cancel: expected state conflict, got %v", err)
	}
}

func TestCandidatesImmutableAfterFreeze(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)
	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.SetCandidates("T1", cands("lib", "1.1.0"), 2)
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("expected state conflict replacing frozen candidates, got %v", err)
	}
}

func TestEditCandidatesRevisionConflict(t *testing.T) {
	f := newFixture(t)
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)

	// 第一次替换成功。
	updated, err := f.svc.SetCandidates("T1", cands("lib", "1.1.0"), train.Revision)
	if err != nil {
		t.Fatalf("set candidates: %v", err)
	}

	// 用旧修订号并发替换 -> 版本冲突，且服务端返回当前修订号。
	_, err = f.svc.SetCandidates("T1", cands("lib", "1.0.0"), train.Revision)
	var ve *Error
	if !errors.As(err, &ve) || ve.Kind != KindVersion {
		t.Fatalf("expected version conflict, got %v", err)
	}
	if ve.Current != updated.Revision {
		t.Fatalf("current revision = %d, want %d", ve.Current, updated.Revision)
	}
}

// ---------------------------------------------------------------------------
// 审批
// ---------------------------------------------------------------------------

func TestApprovalFlow(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)
	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}

	// 非策略角色无权审批。
	if _, _, err := f.svc.Approve("T1", "x", "intruder", 2); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("expected approval conflict for unknown role, got %v", err)
	}
	// 不在成员名单中的人员无权审批。
	if _, _, err := f.svc.Approve("T1", "mallory", "qa", 2); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("expected approval conflict for non-member, got %v", err)
	}

	// 合法审批。
	if _, a, err := f.svc.Approve("T1", "alice", "qa", 2); err != nil {
		t.Fatalf("approve alice: %v", err)
	} else if a.Person != "alice" {
		t.Fatalf("approval = %+v", a)
	}

	// 同一人同角色重复审批幂等：修订号不再增加。
	before, _ := f.svc.GetTrain("T1")
	t2, a2, err := f.svc.Approve("T1", "alice", "qa", before.Revision)
	if err != nil {
		t.Fatalf("idempotent approve: %v", err)
	}
	if t2.Revision != before.Revision {
		t.Fatalf("idempotent approve bumped revision %d -> %d", before.Revision, t2.Revision)
	}
	if !a2.At.Equal(before.Approvals[0].At) {
		t.Fatalf("idempotent approve returned a different record: %+v vs %+v", a2, before.Approvals[0])
	}

	// 未满足全部角色前不能放行。
	_, err = f.svc.ReleaseTrain("T1", t2.Revision)
	if !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("expected approval conflict releasing early, got %v", err)
	}

	// manager 也通过。
	t3, _, err := f.svc.Approve("T1", "bob", "manager", t2.Revision)
	if err != nil {
		t.Fatalf("approve bob: %v", err)
	}

	sum, err := f.svc.ApprovalSummary("T1")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.AllSatisfied {
		t.Fatalf("summary not satisfied: %+v", sum)
	}

	// 放行成功。
	res, err := f.svc.ReleaseTrain("T1", t3.Revision)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.OutboxID != 1 {
		t.Fatalf("outbox id = %d, want 1", res.OutboxID)
	}
}

func TestApprovalUsesFrozenPolicySnapshot(t *testing.T) {
	f := newFixture(t)
	// 冻结时：qa 需要 alice。
	if _, err := f.svc.SetPolicy([]ApprovalRule{
		{Role: "qa", Members: []string{"alice"}, Threshold: 1},
	}); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)
	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}

	// 冻结后策略被改写：qa 只剩 zoe，且新增 manager 角色。
	if _, err := f.svc.SetPolicy([]ApprovalRule{
		{Role: "qa", Members: []string{"zoe"}, Threshold: 1},
		{Role: "manager", Members: []string{"bob"}, Threshold: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// 按新策略 zoe 有资格、bob 也需要审批；但本列车按快照执行：
	frozen, _ := f.svc.GetTrain("T1")
	if _, _, err := f.svc.Approve("T1", "zoe", "qa", frozen.Revision); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("zoe should be rejected by frozen snapshot, got %v", err)
	}
	if _, _, err := f.svc.Approve("T1", "alice", "qa", frozen.Revision); err != nil {
		t.Fatalf("alice should still be approved by snapshot: %v", err)
	}
	if _, _, err := f.svc.Approve("T1", "bob", "manager", 3); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("manager role should not exist in snapshot, got %v", err)
	}

	// 只有快照中的 qa 一个角色，alice 通过即可放行。
	res, err := f.svc.ReleaseTrain("T1", 3)
	if err != nil {
		t.Fatalf("release under snapshot policy: %v", err)
	}
	if res.TrainID != "T1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestApprovalThresholdNeedsDistinctPeople(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetPolicy([]ApprovalRule{
		{Role: "qa", Members: []string{"alice", "erin"}, Threshold: 2},
	}); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)
	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Approve("T1", "alice", "qa", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReleaseTrain("T1", 3); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("release with threshold 2 and 1 approver: %v", err)
	}
	if _, _, err := f.svc.Approve("T1", "erin", "qa", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReleaseTrain("T1", 4); err != nil {
		t.Fatalf("release with threshold met: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 放行 / 取消终态互斥
// ---------------------------------------------------------------------------

func TestReleaseIdempotentAndSingleOutbox(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)
	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Approve("T1", "alice", "qa", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Approve("T1", "bob", "manager", 3); err != nil {
		t.Fatal(err)
	}

	first, err := f.svc.ReleaseTrain("T1", 4)
	if err != nil {
		t.Fatalf("first release: %v", err)
	}

	// 重复放行（哪怕用过期修订号）返回同一份原结果，不报错。
	second, err := f.svc.ReleaseTrain("T1", 4)
	if err != nil {
		t.Fatalf("duplicate release should be idempotent: %v", err)
	}
	if second.OutboxID != first.OutboxID || second.Revision != first.Revision ||
		!second.ReleasedAt.Equal(first.ReleasedAt) {
		t.Fatalf("duplicate release returned different result:\n first=%+v\nsecond=%+v", first, second)
	}

	// outbox 只有一条事件。
	events := f.svc.ListOutbox()
	if len(events) != 1 {
		t.Fatalf("outbox length = %d, want 1", len(events))
	}
	if events[0].TrainID != "T1" || events[0].Type != "train.released" {
		t.Fatalf("unexpected outbox event: %+v", events[0])
	}

	// 已放行不可取消。
	if _, err := f.svc.CancelTrain("T1", "late", first.Revision); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("cancel after release: expected state conflict, got %v", err)
	}
}

func TestCancelTerminalAndBlocksRelease(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatal(err)
	}
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)
	if _, err := f.svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}
	cancelled, err := f.svc.CancelTrain("T1", "not needed", 2)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != StateCancelled {
		t.Fatalf("state = %s", cancelled.State)
	}

	// 已取消不可放行。
	if _, err := f.svc.ReleaseTrain("T1", cancelled.Revision); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("release after cancel: expected state conflict, got %v", err)
	}
	// 重复取消返回状态冲突（终态）。
	if _, err := f.svc.CancelTrain("T1", "again", cancelled.Revision); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("double cancel: expected state conflict, got %v", err)
	}
	// outbox 不应有任何事件。
	if len(f.svc.ListOutbox()) != 0 {
		t.Fatal("cancelled train produced outbox events")
	}
}

// TestReleaseCancelRace 并发放行/取消，要求恰好一个终态生效，绝不产生 outbox
// 与 cancelled 并存的情况。
func TestReleaseCancelRace(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		f := newFixture(t)
		if _, err := f.svc.SetPolicy(standardPolicy()); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("RACE-%d", iter)
		train := f.mustTrain(id, cands("lib", "1.0.0")...)
		if _, err := f.svc.FreezeTrain(id, train.Revision); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.svc.Approve(id, "alice", "qa", 2); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.svc.Approve(id, "bob", "manager", 3); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var releaseErr, cancelErr error
		go func() { defer wg.Done(); _, releaseErr = f.svc.ReleaseTrain(id, 4) }()
		go func() { defer wg.Done(); _, cancelErr = f.svc.CancelTrain(id, "race", 4) }()
		wg.Wait()

		final, _ := f.svc.GetTrain(id)
		outboxCount := 0
		for _, e := range f.svc.ListOutbox() {
			if e.TrainID == id {
				outboxCount++
			}
		}
		switch final.State {
		case StateReleased:
			// 输家可能先撞修订号（version conflict），也可能在状态机处被拒（state conflict）。
			if cancelErr == nil ||
				(!errors.Is(cancelErr, ErrStateConflict) && !errors.Is(cancelErr, ErrVersionConflict)) {
				t.Fatalf("iter %d: released but cancel did not report conflict: %v", iter, cancelErr)
			}
			if releaseErr != nil {
				t.Fatalf("iter %d: released but release reported error: %v", iter, releaseErr)
			}
			if outboxCount != 1 {
				t.Fatalf("iter %d: released with %d outbox events", iter, outboxCount)
			}
		case StateCancelled:
			if releaseErr == nil ||
				(!errors.Is(releaseErr, ErrStateConflict) && !errors.Is(releaseErr, ErrVersionConflict)) {
				t.Fatalf("iter %d: cancelled but release did not report conflict: %v", iter, releaseErr)
			}
			if outboxCount != 0 {
				t.Fatalf("iter %d: cancelled but produced %d outbox events", iter, outboxCount)
			}
		default:
			t.Fatalf("iter %d: unexpected final state %s", iter, final.State)
		}
	}
}

// TestConcurrentCandidateEdits 并发替换候选，恰好一次成功，其余全部版本冲突，
// 不会丢失为零值或产生覆盖写。
func TestConcurrentCandidateEdits(t *testing.T) {
	f := newFixture(t)
	train := f.mustTrain("T1", cands("lib", "1.0.0")...)

	var wg sync.WaitGroup
	success := 0
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.SetCandidates("T1", cands("lib", "1.1.0"), train.Revision)
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(err, ErrVersionConflict) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("concurrent edits: %d winners, want 1", success)
	}
	after, _ := f.svc.GetTrain("T1")
	if after.Revision != 2 || len(after.Candidates) != 1 || after.Candidates[0].Version != "1.1.0" {
		t.Fatalf("state after concurrent edits: rev=%d cands=%+v", after.Revision, after.Candidates)
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	svc, err := NewService(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterComponent("lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterVersion("lib", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPolicy(standardPolicy()); err != nil {
		t.Fatal(err)
	}
	train, err := svc.CreateTrain("T1", cands("lib", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FreezeTrain("T1", train.Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Approve("T1", "alice", "qa", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Approve("T1", "bob", "manager", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseTrain("T1", 4); err != nil {
		t.Fatal(err)
	}

	// 重新打开，状态（含快照、审批、放行结果、outbox）应完整恢复。
	reopened, err := NewService(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetTrain("T1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReleased || got.ReleaseResult == nil || got.ReleaseResult.OutboxID != 1 {
		t.Fatalf("reloaded train wrong: %+v", got)
	}
	if len(got.PolicySnapshot) != 2 || len(got.Approvals) != 2 {
		t.Fatalf("snapshots lost on reload: %+v", got)
	}
	events := reopened.ListOutbox()
	if len(events) != 1 {
		t.Fatalf("outbox after reload: %+v", events)
	}
	if events[0].Published {
		t.Fatal("fresh event should not be marked published")
	}

	// 重新放行仍返回原结果且不新增 outbox。
	res, err := reopened.ReleaseTrain("T1", 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutboxID != 1 || len(reopened.ListOutbox()) != 1 {
		t.Fatal("re-release after reload created a new outbox event")
	}
}
