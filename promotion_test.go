package releasetrain

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// releaseACompatibleTrain 完成“登记版本→装候选→冻结→审批齐→放行”，返回 trainID。
func releaseACompatibleTrain(t *testing.T, svc *Service) string {
	t.Helper()
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)
	if _, err := svc.Release(tr.ID, 0, ""); err != nil {
		t.Fatalf("release: %v", err)
	}
	return tr.ID
}

func mustCreatePromotion(t *testing.T, svc *Service, trainID string) *Promotion {
	t.Helper()
	p, err := svc.CreatePromotion(trainID, "")
	if err != nil {
		t.Fatalf("create promotion: %v", err)
	}
	return p
}

// successReceipts 为快照内每个组件生成成功回执。
func successReceipts(p *Promotion) []ReceiptEntry {
	entries := make([]ReceiptEntry, 0, len(p.Snapshot()))
	for comp := range p.Snapshot() {
		entries = append(entries, ReceiptEntry{Component: comp, Success: true})
	}
	return entries
}

func deployEnvFully(t *testing.T, svc *Service, promoID, env, worker string, attemptNo, leaseNo int) {
	t.Helper()
	p, err := svc.GetPromotion(promoID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.SubmitReceipts(promoID, attemptNo, leaseNo, env, successReceipts(p), "")
	if err != nil {
		t.Fatalf("submit receipts %s: %v", env, err)
	}
	if !out.Promoted {
		t.Fatalf("env %s not promoted: %+v", env, out)
	}
}

// ---- 1. 创建门槛：未放行不能晋级；快照/顺序/门槛在创建时冻结 ----

func TestPromotionRequiresReleasedTrain(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)

	// editing 列车不能晋级。
	if _, err := svc.CreatePromotion(tr.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("editing train promotion: %v", err)
	}
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	// frozen 但未放行仍不能晋级。
	if _, err := svc.CreatePromotion(tr.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("frozen (not released) promotion: %v", err)
	}
	if _, err := svc.Cancel(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 已取消列车不能晋级。
	if _, err := svc.CreatePromotion(tr.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("cancelled train promotion: %v", err)
	}
}

func TestOnePromotionPerTrain(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	mustCreatePromotion(t, svc, trainID)
	if _, err := svc.CreatePromotion(trainID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("second promotion: %v", err)
	}
}

func TestPromotionSnapshotDependencyIncomplete(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	// 白箱：直接从底层登记库删掉快照中的一个版本，制造“依赖快照已不完整”。
	if err := svc.store.Update(func(st *storedState) error {
		delete(st.versions["payment"], "1.2.0")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreatePromotion(trainID, "")
	var depErr *DependencyError
	if !errors.As(err, &depErr) {
		t.Fatalf("want dependency error for incomplete snapshot, got %v", err)
	}
	if len(depErr.Problems) != 1 {
		t.Fatalf("problems = %v", depErr.Problems)
	}
}

func TestPromotionFreezesEnvironmentOrderAndPolicies(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	// 默认顺序 dev → staging → production。
	p := mustCreatePromotion(t, svc, trainID)
	if got := p.Environments(); len(got) != 3 || got[0] != "dev" || got[2] != "production" {
		t.Fatalf("envs = %v", got)
	}
	// 创建后改环境定义：不影响已创建活动（仍只有原 3 个环境）。
	if err := svc.PutEnvironmentPolicy(&EnvironmentPolicy{Environments: []EnvPolicy{
		{Name: "only"},
	}}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetPromotion(p.ID)
	if envs := got.Environments(); len(envs) != 3 {
		t.Fatalf("existing promotion mutated by policy change: %v", envs)
	}
}

// ---- 严格顺序 + 审批门槛 ----

func gatedEnvironmentPolicy(t *testing.T, svc *Service) {
	t.Helper()
	err := svc.PutEnvironmentPolicy(&EnvironmentPolicy{Environments: []EnvPolicy{
		{Name: "dev", Rules: []ApprovalRule{{Role: "qa", Need: 1}},
			Approvers: []Approver{{Person: "alice", Role: "qa"}}},
		{Name: "staging", Rules: []ApprovalRule{{Role: "manager", Need: 1}},
			Approvers: []Approver{{Person: "bob", Role: "manager"}}},
		{Name: "production",
			Rules:     []ApprovalRule{{Role: "qa", Need: 1}, {Role: "manager", Need: 1}},
			Approvers: []Approver{{Person: "alice", Role: "qa"}, {Person: "bob", Role: "manager"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentOrderAndApprovalGates(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	gatedEnvironmentPolicy(t, svc)
	p := mustCreatePromotion(t, svc, trainID)

	view, _ := svc.GetPromotionView(p.ID)
	dev := view.Attempts[0].Environments[0]
	if dev.Status != "waiting_approval" {
		t.Fatalf("dev status = %s, want waiting_approval", dev.Status)
	}
	if len(dev.MissingApprovals) == 0 {
		t.Fatal("dev should report missing approval")
	}

	// staging 在前序未完成 + 自身未审批时，领取必须被状态冲突挡住（不能越过前一环境）。
	if _, err := svc.ClaimDeployment(p.ID, "staging", "w"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("claim staging out of order: %v", err)
	}

	// 无资格角色审批被拒。
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "dev", Person: "bob", Role: "manager",
	}); !errors.Is(err, ErrApproval) {
		t.Fatalf("bob is not qa for dev: %v", err)
	}
	// alice(qa) 审批 dev；重复审批幂等。
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "dev", Person: "alice", Role: "qa",
	}); err != nil {
		t.Fatalf("approve dev: %v", err)
	}
	v1, _ := svc.GetPromotion(p.ID)
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "dev", Person: "alice", Role: "qa",
	}); err != nil {
		t.Fatalf("duplicate env approval: %v", err)
	}
	v2, _ := svc.GetPromotion(p.ID)
	if v2.Version != v1.Version {
		t.Fatal("duplicate env approval bumped version")
	}

	lease, err := svc.ClaimDeployment(p.ID, "dev", "w1")
	if err != nil {
		t.Fatalf("claim dev after approval: %v", err)
	}
	deployEnvFully(t, svc, p.ID, "dev", "w1", lease.AttemptNo, lease.LeaseNo)

	// dev 已晋级但 staging 审批未齐：仍不可领取。
	if _, err := svc.ClaimDeployment(p.ID, "staging", "w"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("claim staging without manager approval: %v", err)
	}
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "staging", Person: "bob", Role: "manager",
	}); err != nil {
		t.Fatalf("approve staging: %v", err)
	}
	sl, err := svc.ClaimDeployment(p.ID, "staging", "w1")
	if err != nil {
		t.Fatalf("claim staging: %v", err)
	}
	deployEnvFully(t, svc, p.ID, "staging", "w1", sl.AttemptNo, sl.LeaseNo)

	// production 需要两个角色都审批。
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "production", Person: "alice", Role: "qa",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimDeployment(p.ID, "production", "w"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("claim production with one of two roles: %v", err)
	}
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "production", Person: "bob", Role: "manager",
	}); err != nil {
		t.Fatal(err)
	}
	pl, err := svc.ClaimDeployment(p.ID, "production", "w1")
	if err != nil {
		t.Fatal(err)
	}
	deployEnvFully(t, svc, p.ID, "production", "w1", pl.AttemptNo, pl.LeaseNo)

	final, _ := svc.GetPromotion(p.ID)
	if final.State() != PromSucceeded {
		t.Fatalf("final state = %s", final.State())
	}
}

// ---- 2. 部分组件失败 → 记录实际结果 → 回退已更新组件 → 新尝试沿用原快照 ----

func TestPartialFailureRollbackAndRetryWithSameSnapshot(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)
	snapshotID := p.SnapshotID()
	snapshot := p.Snapshot()

	lease, err := svc.ClaimDeployment(p.ID, "dev", "w1")
	if err != nil {
		t.Fatal(err)
	}
	// 先发两个成功、一个失败。
	entries := []ReceiptEntry{
		{Component: "order", Success: true},
		{Component: "gateway", Success: true},
		{Component: "payment", Success: false, Message: "boom"},
	}
	out, err := svc.SubmitReceipts(p.ID, lease.AttemptNo, lease.LeaseNo, "dev", entries, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Promoted || out.Status != "rolling_back" {
		t.Fatalf("partial failure outcome = %+v", out)
	}

	// 绝不能把混合状态当成功：无 environment.promoted 事件，现网未整体更新。
	events := envEvents(t, svc)
	if len(events) != 0 {
		t.Fatalf("failed deploy produced outbox: %+v", events)
	}
	cur, _ := svc.GetPromotion(p.ID)
	a := cur.CurrentAttempt()
	if a.Status != AttemptRollingBack {
		t.Fatalf("attempt status = %s", a.Status)
	}
	env, _ := a.env("dev")
	if len(env.rollback) != 2 {
		t.Fatalf("rollback units = %d, want 2 (order,gateway)", len(env.rollback))
	}

	// 回退两个已更新组件；第二个先报失败可重试。
	u1, err := svc.ClaimRollback(p.ID, "rb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportRollback(p.ID, u1.AttemptNo, u1.LeaseNo, "dev", u1.Component, false, "flaky"); err != nil {
		t.Fatal(err)
	}
	u1again, err := svc.ClaimRollback(p.ID, "rb")
	if err != nil {
		t.Fatal(err)
	}
	if u1again.Component != u1.Component || u1again.LeaseNo != u1.LeaseNo+1 {
		t.Fatalf("retry claim = %+v vs %+v", u1again, u1)
	}
	rbDone, err := svc.ReportRollback(p.ID, u1again.AttemptNo, u1again.LeaseNo, "dev", u1again.Component, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if rbDone.RollbackDone {
		t.Fatal("rollback should not be done with one unit left")
	}
	u2, err := svc.ClaimRollback(p.ID, "rb")
	if err != nil {
		t.Fatal(err)
	}
	final, err := svc.ReportRollback(p.ID, u2.AttemptNo, u2.LeaseNo, "dev", u2.Component, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !final.AttemptFailed {
		t.Fatalf("after rollback attempt should be failed: %+v", final)
	}

	// 回退完成前不能开新尝试；现在可以，且必须沿用同一快照。
	res, np, err := svc.StartNewAttempt(p.ID, "")
	if err != nil {
		t.Fatalf("new attempt: %v", err)
	}
	if res.AttemptNo != 2 {
		t.Fatalf("new attempt no = %d", res.AttemptNo)
	}
	if np.SnapshotID() != snapshotID {
		t.Fatalf("snapshot changed: %s vs %s", np.SnapshotID(), snapshotID)
	}
	if len(np.Snapshot()) != len(snapshot) {
		t.Fatal("new attempt snapshot size differs")
	}

	// dev 已回退干净，新尝试中需重新部署。
	l2, err := svc.ClaimDeployment(p.ID, "dev", "w2")
	if err != nil {
		t.Fatal(err)
	}
	if l2.AttemptNo != 2 {
		t.Fatalf("lease attempt = %d, want 2", l2.AttemptNo)
	}
	deployEnvFully(t, svc, p.ID, "dev", "w2", l2.AttemptNo, l2.LeaseNo)
	for _, name := range []string{"staging", "production"} {
		l, err := svc.ClaimDeployment(p.ID, name, "w2")
		if err != nil {
			t.Fatalf("claim %s: %v", name, err)
		}
		deployEnvFully(t, svc, p.ID, name, "w2", l.AttemptNo, l.LeaseNo)
	}
	done, _ := svc.GetPromotion(p.ID)
	if done.State() != PromSucceeded {
		t.Fatalf("state = %s", done.State())
	}
	if ev := envEvents(t, svc); len(ev) != 3 {
		t.Fatalf("environment.promoted events = %d, want one per env (3)", len(ev))
	}
}

func TestNewAttemptCarriesAlreadyPromotedEnvironment(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)

	// dev 成功晋级。
	dl, _ := svc.ClaimDeployment(p.ID, "dev", "w")
	deployEnvFully(t, svc, p.ID, "dev", "w", dl.AttemptNo, dl.LeaseNo)
	devEventID := ""
	view, _ := svc.GetPromotionView(p.ID)
	for _, e := range view.Attempts[0].Environments {
		if e.Name == "dev" {
			devEventID = e.EventID
		}
	}

	// staging 两个成功一个失败 → 回退 → 尝试失败。
	sl, _ := svc.ClaimDeployment(p.ID, "staging", "w")
	_, err := svc.SubmitReceipts(p.ID, sl.AttemptNo, sl.LeaseNo, "staging", []ReceiptEntry{
		{Component: "order", Success: true},
		{Component: "gateway", Success: true},
		{Component: "payment", Success: false, Message: "x"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	drainRollback(t, svc, p.ID)

	if _, _, err := svc.StartNewAttempt(p.ID, ""); err != nil {
		t.Fatalf("new attempt: %v", err)
	}
	view2, _ := svc.GetPromotionView(p.ID)
	if len(view2.Attempts) != 2 {
		t.Fatalf("attempts = %d", len(view2.Attempts))
	}
	var dev2 EnvView
	for _, e := range view2.Attempts[1].Environments {
		if e.Name == "dev" {
			dev2 = e
		}
	}
	if dev2.Status != "promoted" || dev2.CarriedFromAttempt != 1 || dev2.EventID != devEventID {
		t.Fatalf("dev not carried: %+v", dev2)
	}
	// dev 已继承，staging 直接可领取。
	if _, err := svc.ClaimDeployment(p.ID, "staging", "w"); err != nil {
		t.Fatalf("staging should be ready: %v", err)
	}
	deployEnvFully(t, svc, p.ID, "staging", "w", 2, mustLeaseNo(t, svc, p.ID, "staging"))
	pl, _ := svc.ClaimDeployment(p.ID, "production", "w")
	deployEnvFully(t, svc, p.ID, "production", "w", pl.AttemptNo, pl.LeaseNo)

	// dev 的 outbox 仍然只有一条（继承不重复写）。
	count := 0
	for _, ev := range envEvents(t, svc) {
		if string(ev.Payload) != "" && envNameOf(ev) == "dev" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dev events = %d, want 1", count)
	}
	final, _ := svc.GetPromotion(p.ID)
	if final.State() != PromSucceeded {
		t.Fatalf("state = %s", final.State())
	}
}

func mustLeaseNo(t *testing.T, svc *Service, promoID, env string) int {
	t.Helper()
	view, err := svc.GetPromotionView(promoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range view.Attempts {
		for _, e := range a.Environments {
			if e.Name == env && e.Lease != nil {
				return e.Lease.LeaseNo
			}
		}
	}
	t.Fatalf("no active lease for %s", env)
	return 0
}

func envNameOf(ev OutboxEvent) string {
	var payload struct {
		Environment string `json:"environment"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)
	return payload.Environment
}

func envEvents(t *testing.T, svc *Service) []OutboxEvent {
	t.Helper()
	events, err := svc.PendingOutbox()
	if err != nil {
		t.Fatal(err)
	}
	out := []OutboxEvent{}
	for _, ev := range events {
		if ev.Type == "environment.promoted" {
			out = append(out, ev)
		}
	}
	return out
}

func drainRollback(t *testing.T, svc *Service, promoID string) {
	t.Helper()
	for {
		u, err := svc.ClaimRollback(promoID, "rb")
		if errors.Is(err, ErrNotFound) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ReportRollback(promoID, u.AttemptNo, u.LeaseNo,
			u.Environment, u.Component, true, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- 3. 租约 / 尝试号 fencing：接管、旧回执失效、不能跨尝试 ----

func TestLeaseFencingOnTakeoverAndAttempt(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)

	// 同一工作者重复领取：返回相同租约号。
	l1, err := svc.ClaimDeployment(p.ID, "dev", "w1")
	if err != nil {
		t.Fatal(err)
	}
	l1again, err := svc.ClaimDeployment(p.ID, "dev", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if l1again.LeaseNo != l1.LeaseNo || l1again.AttemptNo != l1.AttemptNo {
		t.Fatalf("same worker re-claim changed lease: %+v vs %+v", l1, l1again)
	}

	// w1 先提交一个成功回执。
	if _, err := svc.SubmitReceipts(p.ID, 1, 1, "dev", []ReceiptEntry{
		{Component: "order", Success: true},
	}, ""); err != nil {
		t.Fatal(err)
	}

	// w2 接管：租约号变为 2。
	l2, err := svc.ClaimDeployment(p.ID, "dev", "w2")
	if err != nil || !l2.TakenOver || l2.LeaseNo != 2 {
		t.Fatalf("takeover lease = %+v err=%v", l2, err)
	}

	// w1 用旧租约号提交 → 拒绝，不能覆盖接管后状态。
	if _, err := svc.SubmitReceipts(p.ID, 1, 1, "dev", []ReceiptEntry{
		{Component: "payment", Success: true},
	}, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale lease receipt accepted: %v", err)
	}
	// w2 用新租约提交全部组件成功（旧租约记录的部分结果已被接管清空，需整列车重交）。
	out, err := svc.SubmitReceipts(p.ID, 1, 2, "dev", successReceipts(p), "")
	if err != nil {
		t.Fatalf("new lease receipts: %v", err)
	}
	if !out.Promoted {
		t.Fatalf("dev not promoted by new lease: %+v", out)
	}
}

func TestConcurrentClaimsHighestLeaseWins(t *testing.T) {
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)

	const n = 10
	var wg sync.WaitGroup
	leases := make(chan *DeploymentLease, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			l, err := svc.ClaimDeployment(p.ID, "dev", fmt.Sprintf("w%d", i))
			if err == nil {
				leases <- l
			}
		}()
	}
	wg.Wait()
	close(leases)
	maxLease := 0
	for l := range leases {
		if l.LeaseNo > maxLease {
			maxLease = l.LeaseNo
		}
	}
	if maxLease != n {
		t.Fatalf("max lease = %d, want %d", maxLease, n)
	}
	// 只有最高租约号能提交；任何更低租约号都被拒。
	if _, err := svc.SubmitReceipts(p.ID, 1, 1, "dev",
		successReceipts(p), ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("lowest lease after contention accepted: %v", err)
	}
	out, err := svc.SubmitReceipts(p.ID, 1, maxLease, "dev", successReceipts(p), "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Promoted {
		t.Fatalf("highest lease did not promote: %+v", out)
	}
}

// ---- 4. 幂等：重复回执 / 请求重试 / 每环境一条稳定 outbox ----

func TestReceiptIdempotencyAndSingleOutboxPerEnvironment(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w")

	// 分批提交：第一批两个组件。
	first := []ReceiptEntry{{Component: "order", Success: true}, {Component: "payment", Success: true}}
	r1, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", first, "rc-key")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Promoted {
		t.Fatal("partial batch should not promote")
	}
	// 同一 request_id 重试：返回已有结果，不重复计数。
	r1rep, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", first, "rc-key")
	if err != nil {
		t.Fatal(err)
	}
	if !r1rep.Idempotent || r1rep.Promoted {
		t.Fatalf("retry replay = %+v", r1rep)
	}
	// 无 request_id 时重复相同组件回执也幂等（保留首个结果），不报错。
	if _, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev",
		[]ReceiptEntry{{Component: "order", Success: true}}, ""); err != nil {
		t.Fatalf("duplicate component receipt: %v", err)
	}

	// 补齐最后一个组件 → 晋级，写一条 outbox。
	final, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev",
		[]ReceiptEntry{{Component: "gateway", Success: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !final.Promoted {
		t.Fatalf("final batch = %+v", final)
	}
	// 再次提交整批（含已晋级环境）：幂等，不产生第二条事件。
	replay, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", successReceipts(p), "")
	if err != nil {
		t.Fatalf("receipts after promotion should replay: %v", err)
	}
	if !replay.Promoted || replay.EventID != final.EventID {
		t.Fatalf("post-promotion replay = %+v vs %+v", replay, final)
	}
	if ev := envEvents(t, svc); len(ev) != 1 || ev[0].ID != final.EventID {
		t.Fatalf("dev outbox = %+v", ev)
	}
}

// ---- 3/取消：审批、回执、取消并发 ----

func TestCancelDuringDeploymentTriggersRollback(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)

	l, _ := svc.ClaimDeployment(p.ID, "dev", "w")
	if _, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", []ReceiptEntry{
		{Component: "order", Success: true},
	}, ""); err != nil {
		t.Fatal(err)
	}
	// 已有一个组件更新：取消转入 cancel_requested 并回退。
	cp, started, err := svc.CancelPromotion(p.ID, "")
	if err != nil || !started {
		t.Fatalf("cancel = started%v err=%v", started, err)
	}
	if cp.State() != PromCancelRequested {
		t.Fatalf("state = %s", cp.State())
	}
	// 回退完成前旧部署租约不能再提交（尝试已非 running）。
	if _, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", []ReceiptEntry{
		{Component: "payment", Success: true},
	}, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("receipt after cancel accepted: %v", err)
	}
	drainRollback(t, svc, p.ID)
	got, _ := svc.GetPromotion(p.ID)
	if got.State() != PromCancelled {
		t.Fatalf("state after rollback = %s", got.State())
	}
	// 已取消不能再开新尝试。
	if _, _, err := svc.StartNewAttempt(p.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("new attempt after cancel: %v", err)
	}
	// 重复取消幂等。
	again, started2, err := svc.CancelPromotion(p.ID, "")
	if err != nil || started2 || again.State() != PromCancelled {
		t.Fatalf("repeat cancel = %+v started=%v err=%v", again, started2, err)
	}
}

func TestCancelWithNoUpdatedComponentsIsImmediate(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)
	if _, err := svc.ClaimDeployment(p.ID, "dev", "w"); err != nil {
		t.Fatal(err)
	}
	// 租约已发但零成功回执：立即取消，无需回退。
	cp, started, err := svc.CancelPromotion(p.ID, "")
	if err != nil || started || cp.State() != PromCancelled {
		t.Fatalf("cancel = state %s started=%v err=%v", cp.State(), started, err)
	}
}

func TestCancelAfterSuccessConflicts(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w")
	deployEnvFully(t, svc, p.ID, "dev", "w", l.AttemptNo, l.LeaseNo)
	for _, name := range []string{"staging", "production"} {
		ll, _ := svc.ClaimDeployment(p.ID, name, "w")
		deployEnvFully(t, svc, p.ID, name, "w", ll.AttemptNo, ll.LeaseNo)
	}
	if _, _, err := svc.CancelPromotion(p.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("cancel after success: %v", err)
	}
}

func TestSuccessReceiptAtWrongVersionRejected(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w")
	wrong := v("9.9.9")
	_, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", []ReceiptEntry{
		{Component: "order", Success: true, DeployedVersion: &wrong},
	}, "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("success at non-target version should be invalid argument, got %v", err)
	}
	// 被拒后该组件不应有记录，仍可用目标版本正常提交。
	entries := successReceipts(p)
	out, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", entries, "")
	if err != nil {
		t.Fatalf("valid receipts after rejected one: %v", err)
	}
	if !out.Promoted {
		t.Fatal("env should promote with correct snapshot versions")
	}
}

// ---- 5. 查询视图 ----

func TestPromotionViewShowsDetailsAndBlocking(t *testing.T) {
	svc := newTestService(t)
	trainID := releaseACompatibleTrain(t, svc)
	gatedEnvironmentPolicy(t, svc)
	p := mustCreatePromotion(t, svc, trainID)

	view, err := svc.GetPromotionView(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Snapshot["payment"] != "1.2.0" {
		t.Fatalf("snapshot = %v", view.Snapshot)
	}
	dev := view.Attempts[0].Environments[0]
	if dev.TargetVersions["order"] != "2.1.0" {
		t.Fatalf("targets = %v", dev.TargetVersions)
	}
	if dev.BlockingReason == "" {
		t.Fatal("dev should show approval blocking reason")
	}
	if len(dev.ApprovalPolicy.Rules) != 1 {
		t.Fatalf("dev approval policy basis missing: %+v", dev.ApprovalPolicy)
	}
	// staging 阻断原因应指向前序环境。
	var staging EnvView
	for _, e := range view.Attempts[0].Environments {
		if e.Name == "staging" {
			staging = e
		}
	}
	if staging.BlockingReason == "" {
		t.Fatal("staging should be blocked by previous environment")
	}

	// 审批并领取 dev，检查执行前版本与租约视图。
	if _, err := svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: p.ID, Environment: "dev", Person: "alice", Role: "qa"}); err != nil {
		t.Fatal(err)
	}
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w1")
	// 部署前所有组件版本应为空（此前不存在）。
	v2, _ := svc.GetPromotionView(p.ID)
	d := v2.Attempts[0].Environments[0]
	if len(d.BeforeVersions) != 3 {
		t.Fatalf("before versions = %v", d.BeforeVersions)
	}
	for _, ver := range d.BeforeVersions {
		if ver != "" {
			t.Fatalf("fresh env before version should be empty, got %q", ver)
		}
	}
	if d.Lease == nil || d.Lease.LeaseNo != l.LeaseNo || d.Lease.Worker != "w1" {
		t.Fatalf("lease view = %+v", d.Lease)
	}

	// 部分回执后视图应展示组件实际结果（order 成功、payment/gateway 失败 → 触发回退）。
	if _, err := svc.SubmitReceipts(p.ID, 1, 1, "dev", []ReceiptEntry{
		{Component: "order", Success: true},
		{Component: "payment", Success: false, Message: "boom"},
		{Component: "gateway", Success: false, Message: "down"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	v3, _ := svc.GetPromotionView(p.ID)
	d3 := v3.Attempts[0].Environments[0]
	if len(d3.Results) != 3 {
		t.Fatalf("results = %v", d3.Results)
	}
	ok, failed := 0, 0
	for _, r := range d3.Results {
		if r.Success {
			ok++
		} else {
			failed++
		}
	}
	if ok != 1 || failed != 2 {
		t.Fatalf("result tally ok=%d failed=%d (actual results must be recorded)", ok, failed)
	}
	if d3.Status != "rolling_back" {
		t.Fatalf("status after partial failure = %s", d3.Status)
	}
	// 回退单元的进度（pending）应可见。
	drainRollback(t, svc, p.ID)
	v4, _ := svc.GetPromotionView(p.ID)
	d4 := v4.Attempts[0].Environments[0]
	if len(d4.Rollback) != 1 {
		t.Fatalf("rollback view = %v (only order succeeded)", d4.Rollback)
	}
	for _, u := range d4.Rollback {
		if u.State != RollbackSucceeded {
			t.Fatalf("rollback unit state = %s", u.State)
		}
	}
}

// ---- 持久化恢复 ----

func TestPromotionPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	trainID := releaseACompatibleTrain(t, svc)
	p := mustCreatePromotion(t, svc, trainID)
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w")
	if _, err := svc.SubmitReceipts(p.ID, 1, l.LeaseNo, "dev", []ReceiptEntry{
		{Component: "order", Success: true},
		{Component: "gateway", Success: true},
		{Component: "payment", Success: false, Message: "boom"},
	}, ""); err != nil {
		t.Fatal(err)
	}

	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(store2)
	got, err := svc2.GetPromotion(p.ID)
	if err != nil {
		t.Fatalf("restore promotion: %v", err)
	}
	if got.SnapshotID() != p.SnapshotID() || len(got.Snapshot()) != 3 {
		t.Fatalf("snapshot not restored: %s %v", got.SnapshotID(), got.Snapshot())
	}
	a := got.CurrentAttempt()
	env, _ := a.env("dev")
	if env.Status != EnvRollingBack || env.leaseNo != 1 || len(env.results) != 3 {
		t.Fatalf("env not restored: status=%s lease=%d results=%d", env.Status, env.leaseNo, len(env.results))
	}
	if len(env.rollback) != 2 {
		t.Fatalf("rollback not restored: %+v", env.rollback)
	}
	// 恢复后可继续领取并完成回退。
	u, err := svc2.ClaimRollback(p.ID, "rb")
	if err != nil {
		t.Fatalf("claim rollback after restart: %v", err)
	}
	if _, err := svc2.ReportRollback(p.ID, u.AttemptNo, u.LeaseNo, "dev", u.Component, true, ""); err != nil {
		t.Fatal(err)
	}
	// 旧租约在恢复后依旧被 fencing 拒绝。
	if _, err := svc2.SubmitReceipts(p.ID, 1, 1, "dev", successReceipts(p), ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale lease after restart: %v", err)
	}
}

// ---- 环境策略校验 ----

func TestEnvironmentPolicyValidation(t *testing.T) {
	svc := newTestService(t)
	badGate := &EnvironmentPolicy{Environments: []EnvPolicy{{
		Name:      "dev",
		Rules:     []ApprovalRule{{Role: "qa", Need: 2}},
		Approvers: []Approver{{Person: "a", Role: "qa"}},
	}}}
	cases := []struct {
		name string
		p    *EnvironmentPolicy
	}{
		{"empty", &EnvironmentPolicy{}},
		{"blank env name", &EnvironmentPolicy{Environments: []EnvPolicy{{Name: "  "}}}},
		{"duplicate env", &EnvironmentPolicy{Environments: []EnvPolicy{{Name: "dev"}, {Name: "dev"}}}},
		{"bad gate", badGate},
	}
	for _, c := range cases {
		if err := svc.PutEnvironmentPolicy(c.p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: want invalid argument, got %v", c.name, err)
		}
	}
	// nil 恢复默认。
	if err := svc.PutEnvironmentPolicy(nil); err != nil {
		t.Fatal(err)
	}
	if got := svc.GetEnvironmentPolicy(); len(got.Environments) != 3 {
		t.Fatalf("default policy envs = %d", len(got.Environments))
	}
}
