package releasetrain

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// ---- 测试夹具 ----

// threeEnvPolicy: dev(qa 1) -> staging(qa 1 + manager 1) -> prod(director 1)。
func threeEnvPolicy(t *testing.T, svc *Service) {
	t.Helper()
	pol := &PromotionPolicy{Environments: []EnvPolicy{
		{
			Name:      "dev",
			Rules:     []ApprovalRule{{Role: "qa", Need: 1}},
			Approvers: []Approver{{Person: "alice", Role: "qa"}},
		},
		{
			Name:  "staging",
			Rules: []ApprovalRule{{Role: "qa", Need: 1}, {Role: "manager", Need: 1}},
			Approvers: []Approver{
				{Person: "alice", Role: "qa"}, {Person: "bob", Role: "manager"},
			},
		},
		{
			Name:  "prod",
			Rules: []ApprovalRule{{Role: "director", Need: 1}},
			Approvers: []Approver{
				{Person: "carol", Role: "director"},
			},
		},
	}}
	if err := svc.PutPromotionPolicy(pol); err != nil {
		t.Fatal(err)
	}
}

// noGatePolicy: dev -> prod，两个环境都不需要审批（直接 ready）。
func noGatePolicy(t *testing.T, svc *Service) {
	t.Helper()
	pol := &PromotionPolicy{Environments: []EnvPolicy{
		{Name: "dev"},
		{Name: "prod"},
	}}
	if err := svc.PutPromotionPolicy(pol); err != nil {
		t.Fatal(err)
	}
}

func releasedTrain(t *testing.T, svc *Service) *Train {
	t.Helper()
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)
	if _, err := svc.Release(tr.ID, 0, ""); err != nil {
		t.Fatalf("release: %v", err)
	}
	// 重新获取：快照 ID 在冻结时才生成，编辑期持有的旧指针上没有。
	got, err := svc.GetTrain(tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func envByName(p *Promotion, name string) *EnvExecution {
	for _, e := range p.Environments() {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// deployAll 以给定租约把快照内全部组件报成功，返回最后一条回执结果。
func deployAll(t *testing.T, svc *Service, promoID, env string, l *DeployLease) *DeployReceiptResult {
	t.Helper()
	p, err := svc.GetPromotion(promoID)
	if err != nil {
		t.Fatal(err)
	}
	var last *DeployReceiptResult
	for comp, ver := range p.Snapshot() {
		vv := ver
		last, err = svc.ReportDeployment(DeployReceiptInput{
			PromotionID: promoID, Environment: env, Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
			Component: comp, Success: true, Version: &vv,
		})
		if err != nil {
			t.Fatalf("deploy %s: %v", comp, err)
		}
	}
	return last
}

// rollbackAll 领取回退并把所有任务报成功。
func rollbackAll(t *testing.T, svc *Service, promoID, env, worker string) {
	t.Helper()
	l, err := svc.ClaimRollback(promoID, env, worker)
	if err != nil {
		t.Fatalf("claim rollback: %v", err)
	}
	for _, task := range l.Tasks {
		_, err := svc.ReportRollback(RollbackReceiptInput{
			PromotionID: promoID, Environment: env, Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
			Component: task.Component, Status: RollbackSucceeded,
		})
		if err != nil {
			t.Fatalf("rollback %s: %v", task.Component, err)
		}
	}
}

// ---- 场景测试 ----

func TestPromotionPolicyValidation(t *testing.T) {
	svc := newTestService(t)
	// 空策略。
	if err := svc.PutPromotionPolicy(&PromotionPolicy{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty policy: %v", err)
	}
	// 环境名重复。
	err := svc.PutPromotionPolicy(&PromotionPolicy{Environments: []EnvPolicy{{Name: "dev"}, {Name: "dev"}}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dup env: %v", err)
	}
	// 门槛非法（need 人数不足）。
	err = svc.PutPromotionPolicy(&PromotionPolicy{Environments: []EnvPolicy{{
		Name: "dev", Rules: []ApprovalRule{{Role: "qa", Need: 2}}, Approvers: []Approver{{Person: "a", Role: "qa"}},
	}}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad gate: %v", err)
	}
}

func TestPromotionRequiresReleasedTrain(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	threeEnvPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)

	// 编辑态：不能创建。
	if _, err := svc.CreatePromotion(tr.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("editing train: %v", err)
	}
	// 冻结未放行：不能开始。
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreatePromotion(tr.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("frozen (not released) train: %v", err)
	}
	// 无晋级策略时也要拒绝（换一列车已放行但先不设策略）。
}

func TestPromotionFullFlowAcrossEnvironments(t *testing.T) {
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	threeEnvPolicy(t, svc)

	p, err := svc.CreatePromotion(tr.ID, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.State() != PromoRunning || p.Attempt != 1 {
		t.Fatalf("new promotion state=%s attempt=%d", p.State(), p.Attempt)
	}
	if p.SnapshotID != tr.SnapshotID() || len(p.Snapshot()) != 3 {
		t.Fatalf("snapshot not frozen: id=%s comps=%v", p.SnapshotID, p.Snapshot())
	}

	// 顺序与门槛：dev 等待审批，staging/prod blocked。
	dev, staging, prod := envByName(p, "dev"), envByName(p, "staging"), envByName(p, "prod")
	if dev.State != EnvAwaitingApproval || staging.State != EnvBlocked || prod.State != EnvBlocked {
		t.Fatalf("initial env states: %s %s %s", dev.State, staging.State, prod.State)
	}

	// 不能越级操作 staging。
	if _, err := svc.ApprovePromotionEnv(p.ID, "staging", "alice", "qa", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("approve blocked env: %v", err)
	}
	if _, err := svc.ClaimDeployment(p.ID, "staging", "w1", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("claim blocked env: %v", err)
	}

	// 无资格审批被拒。
	if _, err := svc.ApprovePromotionEnv(p.ID, "dev", "bob", "manager", ""); !errors.Is(err, ErrApproval) {
		t.Fatalf("ineligible approval: %v", err)
	}

	// dev 审批 -> ready；同一人重复审批幂等。
	p1, err := svc.ApprovePromotionEnv(p.ID, "dev", "alice", "qa", "")
	if err != nil {
		t.Fatal(err)
	}
	if envByName(p1, "dev").State != EnvReady {
		t.Fatalf("dev after approval: %s", envByName(p1, "dev").State)
	}
	p2, _ := svc.ApprovePromotionEnv(p.ID, "dev", "alice", "qa", "")
	if p2.UpdatedAt() != p1.UpdatedAt() {
		t.Fatal("duplicate env approval was not idempotent")
	}

	// dev 部署：领取租约，基线为空（执行前未安装）。
	l, err := svc.ClaimDeployment(p.ID, "dev", "w1", "claim-dev")
	if err != nil {
		t.Fatal(err)
	}
	if l.Epoch != 1 || len(l.Baseline) != 0 || len(l.Components) != 3 {
		t.Fatalf("lease = %+v", l)
	}
	// request_id 重放领取返回同一租约。
	lReplay, err := svc.ClaimDeployment(p.ID, "dev", "w1", "claim-dev")
	if err != nil {
		t.Fatal(err)
	}
	if lReplay.Token != l.Token || lReplay.Epoch != 1 {
		t.Fatalf("claim replay changed lease: %+v", lReplay)
	}

	last := deployAll(t, svc, p.ID, "dev", l)
	if !last.EnvPromoted {
		t.Fatal("dev should be promoted after all components succeed")
	}
	// dev outbox 稳定一条。
	events, _ := svc.PendingOutbox()
	var devEvt *OutboxEvent
	for i := range events {
		if events[i].Type == "environment.promoted" {
			devEvt = &events[i]
		}
	}
	if devEvt == nil {
		t.Fatal("no environment.promoted event for dev")
	}

	// dev 成功后 staging 才开放；prod 仍 blocked。
	p, _ = svc.GetPromotion(p.ID)
	if envByName(p, "dev").State != EnvPromoted {
		t.Fatal("dev not promoted")
	}
	if envByName(p, "staging").State != EnvAwaitingApproval {
		t.Fatalf("staging = %s", envByName(p, "staging").State)
	}
	if envByName(p, "prod").State != EnvBlocked {
		t.Fatalf("prod = %s", envByName(p, "prod").State)
	}

	// staging 门槛：只 qa 审批时还不能领取。
	if _, err := svc.ApprovePromotionEnv(p.ID, "staging", "alice", "qa", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimDeployment(p.ID, "staging", "w1", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("claim staging before manager approval: %v", err)
	}
	if _, err := svc.ApprovePromotionEnv(p.ID, "staging", "bob", "manager", ""); err != nil {
		t.Fatal(err)
	}
	// staging 执行前基线为 dev 已部署版本（installed 按环境隔离，staging 基线仍为空）。
	ls, err := svc.ClaimDeployment(p.ID, "staging", "w1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.Baseline) != 0 {
		t.Fatalf("staging baseline should be empty: %+v", ls.Baseline)
	}
	deployAll(t, svc, p.ID, "staging", ls)

	// prod 审批 + 部署 -> 整体 promoted。
	p, _ = svc.GetPromotion(p.ID)
	if _, err := svc.ApprovePromotionEnv(p.ID, "prod", "carol", "director", ""); err != nil {
		t.Fatal(err)
	}
	lp, err := svc.ClaimDeployment(p.ID, "prod", "w1", "")
	if err != nil {
		t.Fatal(err)
	}
	final := deployAll(t, svc, p.ID, "prod", lp)
	if !final.PromotionDone {
		t.Fatal("promotion should be done after prod")
	}
	p, _ = svc.GetPromotion(p.ID)
	if p.State() != PromoPromoted {
		t.Fatalf("state = %s", p.State())
	}
	// 每个环境恰好一条稳定 outbox。
	events, _ = svc.PendingOutbox()
	count := 0
	for _, e := range events {
		if e.Type == "environment.promoted" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("environment events = %d, want 3", count)
	}
}

func TestPartialFailureTriggersRollbackAndRetryReusesSnapshot(t *testing.T) {
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	noGatePolicy(t, svc)
	p, err := svc.CreatePromotion(tr.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := p.SnapshotID

	// dev: order 成功，payment 失败（部分组件失败）。
	l, err := svc.ClaimDeployment(p.ID, "dev", "w1", "")
	if err != nil {
		t.Fatal(err)
	}
	target := p.Snapshot()
	ov := target["order"]
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
		Component: "order", Success: true, Version: &ov,
	}); err != nil {
		t.Fatal(err)
	}
	failVer := v("1.0.0")
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
		Component: "payment", Success: false, Version: &failVer, Message: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	p, _ = svc.GetPromotion(p.ID)
	dev := envByName(p, "dev")
	if dev.State != EnvRollingBack || p.State() != PromoRunning {
		t.Fatalf("after partial failure env=%s promo=%s", dev.State, p.State())
	}
	// 已更新的 order/payment 需要回退到基线（nil：移除）；未部署的 gateway 无任务。
	if len(dev.Rollback) != 2 {
		t.Fatalf("rollback tasks = %d, want 2: %+v", len(dev.Rollback), dev.Rollback)
	}
	for _, task := range dev.Rollback {
		if task.To != nil {
			t.Fatalf("rollback target for %s = %v, want removal (nil baseline)", task.Component, task.To)
		}
	}
	if len(dev.FailReasons) == 0 {
		t.Fatal("fail reasons not recorded")
	}
	// 混版状态绝不记为成功：dev 未晋级，prod 未开放。
	if envByName(p, "prod").State != EnvBlocked {
		t.Fatal("prod must stay blocked after dev failure")
	}
	// 回退期间部署通道关闭。
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
		Component: "gateway", Success: true, Version: &ov,
	}); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("deploy during rollback: %v", err)
	}

	// 旧部署租约不能提交回退回执（需显式领取回退租约）。
	if _, err := svc.ReportRollback(RollbackReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
		Component: "order", Status: RollbackSucceeded,
	}); !errors.Is(err, ErrLease) {
		t.Fatalf("rollback with stale deploy lease: %v", err)
	}

	// 回退：一个任务先失败可重试，全部成功后落 failed。
	rl, err := svc.ClaimRollback(p.ID, "dev", "rb1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rl.Tasks) != 2 {
		t.Fatalf("rollback claim tasks = %d", len(rl.Tasks))
	}
	// order 回退失败。
	if _, err := svc.ReportRollback(RollbackReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: rl.Token, Attempt: rl.Attempt, Epoch: rl.Epoch,
		Component: "order", Status: RollbackFailed, Message: "agent down",
	}); err != nil {
		t.Fatal(err)
	}
	// payment 回退成功；此时未全部恢复，仍是 rolling_back。
	if _, err := svc.ReportRollback(RollbackReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: rl.Token, Attempt: rl.Attempt, Epoch: rl.Epoch,
		Component: "payment", Status: RollbackSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.GetPromotion(p.ID)
	if p.State() != PromoRunning || envByName(p, "dev").State != EnvRollingBack {
		t.Fatal("promotion should stay running while rollback incomplete")
	}
	// 同租约重报 payment 成功幂等。
	r2, err := svc.ReportRollback(RollbackReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: rl.Token, Attempt: rl.Attempt, Epoch: rl.Epoch,
		Component: "payment", Status: RollbackSucceeded,
	})
	if err != nil || !r2.Idempotent {
		t.Fatalf("duplicate rollback receipt: %+v err=%v", r2, err)
	}
	// order 重试成功 -> failed。
	if _, err := svc.ReportRollback(RollbackReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: rl.Token, Attempt: rl.Attempt, Epoch: rl.Epoch,
		Component: "order", Status: RollbackSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.GetPromotion(p.ID)
	if p.State() != PromoFailed || envByName(p, "dev").State != EnvFailed {
		t.Fatalf("after rollback: promo=%s dev=%s", p.State(), envByName(p, "dev").State)
	}
	// 回退完成后 dev 实际安装版本恢复为执行前（空）。
	if len(p.Installed()["dev"]) != 0 {
		t.Fatalf("dev installed not restored: %v", p.Installed()["dev"])
	}
	if reason := p.BlockingReason(); reason == "" {
		t.Fatal("failed promotion should report a blocking reason")
	}

	// 非失败状态才能重试：直接再重试一次已 failed? 第二次应冲突（已开始新尝试前）。
	// 修复后创建新尝试，沿用同一快照。
	p, err = svc.RetryPromotion(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Attempt != 2 || p.SnapshotID != snapshotID {
		t.Fatalf("retry attempt=%d snap=%s, want 2/%s", p.Attempt, p.SnapshotID, snapshotID)
	}
	dev = envByName(p, "dev")
	if dev.State != EnvReady || len(dev.Approvals) != 0 || len(dev.Results) != 0 || len(dev.Rollback) != 0 {
		t.Fatalf("dev not reset for new attempt: %+v", dev)
	}
	if dev.EventID != "" {
		t.Fatal("failed env must not retain an event id")
	}
	// running 状态不能再次重试。
	if _, err := svc.RetryPromotion(p.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("retry while running: %v", err)
	}

	// 新尝试 dev 全部成功；旧尝试的迟到回执（attempt=1）必须被拒绝。
	l2, err := svc.ClaimDeployment(p.ID, "dev", "w2", "")
	if err != nil {
		t.Fatal(err)
	}
	ov2 := p.Snapshot()["order"]
	oldReceipt, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: "old", Attempt: 1, Epoch: 1,
		Component: "order", Success: true, Version: &ov2,
	})
	if !errors.Is(err, ErrLease) || oldReceipt != nil {
		t.Fatalf("stale attempt receipt: res=%+v err=%v", oldReceipt, err)
	}
	deployAll(t, svc, p.ID, "dev", l2)
	p, _ = svc.GetPromotion(p.ID)
	if envByName(p, "dev").State != EnvPromoted || envByName(p, "dev").Attempt != 2 {
		t.Fatalf("dev after retry: %+v", envByName(p, "dev"))
	}
}

func TestLeaseTakeoverInvalidatesOldReceipts(t *testing.T) {
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	noGatePolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")

	l1, err := svc.ClaimDeployment(p.ID, "dev", "w1", "")
	if err != nil {
		t.Fatal(err)
	}
	// w1 报一个组件成功。
	ov := p.Snapshot()["order"]
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l1.Token, Attempt: 1, Epoch: 1,
		Component: "order", Success: true, Version: &ov,
	}); err != nil {
		t.Fatal(err)
	}

	// w2 接管：epoch 递增；同 worker 续租不递增。
	l2, err := svc.ClaimDeployment(p.ID, "dev", "w2", "")
	if err != nil {
		t.Fatal(err)
	}
	if !l2.Tookover || l2.Epoch != 2 || l2.Token == l1.Token {
		t.Fatalf("takeover lease = %+v", l2)
	}
	l2Again, err := svc.ClaimDeployment(p.ID, "dev", "w2", "")
	if err != nil {
		t.Fatal(err)
	}
	if l2Again.Epoch != 2 || l2Again.Token != l2.Token {
		t.Fatalf("same-worker re-claim rotated lease: %+v", l2Again)
	}

	// w1 的旧租约回执不能覆盖接管后的状态。
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l1.Token, Attempt: 1, Epoch: 1,
		Component: "payment", Success: true, Version: &ov,
	}); !errors.Is(err, ErrLease) {
		t.Fatalf("old lease receipt after takeover: %v", err)
	}
	// 伪造 token / 错误 epoch 同样拒绝。
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l2.Token, Attempt: 1, Epoch: 9,
		Component: "payment", Success: true, Version: &ov,
	}); !errors.Is(err, ErrLease) {
		t.Fatalf("wrong epoch receipt: %v", err)
	}

	// w2 重报 order（覆盖 w1 残留），并完成其余组件 -> dev 晋级。
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l2.Token, Attempt: 1, Epoch: 2,
		Component: "order", Success: true, Version: &ov,
	}); err != nil {
		t.Fatal(err)
	}
	// 同租约重复回执幂等，不产生第二条结果。
	r, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l2.Token, Attempt: 1, Epoch: 2,
		Component: "order", Success: true, Version: &ov,
	})
	if err != nil || !r.Idempotent {
		t.Fatalf("duplicate receipt: %+v err=%v", r, err)
	}
	p, _ = svc.GetPromotion(p.ID)
	if len(envByName(p, "dev").Results) != 1 {
		t.Fatalf("order results = %d, want 1 per component", len(envByName(p, "dev").Results))
	}
	for _, comp := range []string{"payment", "gateway"} {
		vv := p.Snapshot()[comp]
		if _, err := svc.ReportDeployment(DeployReceiptInput{
			PromotionID: p.ID, Environment: "dev", Token: l2.Token, Attempt: 1, Epoch: 2,
			Component: comp, Success: true, Version: &vv,
		}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ = svc.GetPromotion(p.ID)
	if envByName(p, "dev").State != EnvPromoted {
		t.Fatalf("dev = %s, want promoted", envByName(p, "dev").State)
	}
	// 晋级后旧租约迟到回执仍被拒绝；完成租约的重复回执幂等重放。
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l1.Token, Attempt: 1, Epoch: 1,
		Component: "order", Success: true, Version: &ov,
	}); !errors.Is(err, ErrLease) {
		t.Fatalf("old lease receipt after env promoted: %v", err)
	}
	r, err = svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l2.Token, Attempt: 1, Epoch: 2,
		Component: "order", Success: true, Version: &ov,
	})
	if err != nil || !r.Idempotent || !r.EnvPromoted {
		t.Fatalf("completion-lease replay: %+v err=%v", r, err)
	}
}

func TestSuccessfulReceiptMustMatchSnapshotVersion(t *testing.T) {
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	noGatePolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w1", "")

	// 成功回执版本与快照不一致 -> 拒绝（混版不能算成功）。
	wrong := v("1.0.0")
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: 1, Epoch: 1,
		Component: "payment", Success: true, Version: &wrong,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("mixed-version success: %v", err)
	}
	// 快照外组件也拒绝。
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: 1, Epoch: 1,
		Component: "ghost", Success: true,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown component: %v", err)
	}
}

func TestCancelPromotionFlows(t *testing.T) {
	// 1) 审批等待期取消：直接终态，无需回退。
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	threeEnvPolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")
	p, err := svc.CancelPromotion(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.State() != PromoCancelled {
		t.Fatalf("state = %s", p.State())
	}
	// 重复取消幂等；已取消后部署/审批/重试都拒绝。
	p2, err := svc.CancelPromotion(p.ID, "")
	if err != nil || p2.State() != PromoCancelled {
		t.Fatalf("duplicate cancel: %+v %v", p2, err)
	}
	if _, err := svc.RetryPromotion(p.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("retry cancelled: %v", err)
	}

	// 2) 部署进行中取消：cancelling -> 回退完成 -> cancelled。
	svc = newTestService(t)
	tr2 := releasedTrain(t, svc)
	noGatePolicy(t, svc)
	p, _ = svc.CreatePromotion(tr2.ID, "")
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w1", "")
	ov := p.Snapshot()["order"]
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: 1, Epoch: 1,
		Component: "order", Success: true, Version: &ov,
	}); err != nil {
		t.Fatal(err)
	}
	p, err = svc.CancelPromotion(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.State() != PromoCancelling || envByName(p, "dev").State != EnvRollingBack {
		t.Fatalf("after cancel: promo=%s dev=%s", p.State(), envByName(p, "dev").State)
	}
	// 回退未完成时再取消：幂等停留 cancelling。
	p2, err = svc.CancelPromotion(p.ID, "")
	if err != nil || p2.State() != PromoCancelling {
		t.Fatalf("cancel while cancelling: %+v %v", p2, err)
	}
	rollbackAll(t, svc, p.ID, "dev", "rb")
	p, _ = svc.GetPromotion(p.ID)
	if p.State() != PromoCancelled || envByName(p, "dev").State != EnvCancelled {
		t.Fatalf("after rollback cancel: promo=%s dev=%s", p.State(), envByName(p, "dev").State)
	}
	if len(p.Installed()["dev"]) != 0 {
		t.Fatalf("dev installed not restored after cancel: %v", p.Installed()["dev"])
	}

	// 3) 已全部晋级不可取消。
	svc = newTestService(t)
	tr3 := releasedTrain(t, svc)
	noGatePolicy(t, svc)
	p, _ = svc.CreatePromotion(tr3.ID, "")
	for _, env := range []string{"dev", "prod"} {
		l, _ := svc.ClaimDeployment(p.ID, env, "w", "")
		p, _ = svc.GetPromotion(p.ID)
		deployAll(t, svc, p.ID, env, l)
		p, _ = svc.GetPromotion(p.ID)
	}
	if _, err := svc.CancelPromotion(p.ID, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("cancel promoted: %v", err)
	}
}

func TestCancelInterleavesWithApprovalsAndReceipts(t *testing.T) {
	// 审批、部署回执、取消在同一存储上并发发生：事务串行化后，
	// 结果必须自洽（取消赢则无成功回执；回执先赢则进入回退；审批赢则就绪）。
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	tr := releasedTrain(t, svc)
	threeEnvPolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")
	l, err := svc.ClaimDeployment(p.ID, "dev", "w1", "")
	// 审批未齐时不能领取；先由本 goroutine 之外完成审批再领取。
	if err != nil {
		// dev 有门槛：先审批再领取。
		if _, err := svc.ApprovePromotionEnv(p.ID, "dev", "alice", "qa", ""); err != nil {
			t.Fatal(err)
		}
		l, err = svc.ClaimDeployment(p.ID, "dev", "w1", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	ov := p.Snapshot()["order"]

	const n = 12
	var wg sync.WaitGroup
	wg.Add(3 * n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = svc.ApprovePromotionEnv(p.ID, "dev", "alice", "qa", "")
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.ReportDeployment(DeployReceiptInput{
				PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
				Component: "order", Success: true, Version: &ov,
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.CancelPromotion(p.ID, "")
		}()
	}
	wg.Wait()

	got, _ := svc.GetPromotion(p.ID)
	dev := envByName(got, "dev")
	switch got.State() {
	case PromoCancelled:
		// 取消在任何部署动作前赢：环境直接取消，无安装残留。
		if m := got.Installed()["dev"]; len(m) != 0 {
			t.Fatalf("cancelled but dev left install: %v", m)
		}
		if dev.State != EnvCancelled {
			t.Fatalf("cancelled promo but dev = %s", dev.State)
		}
	case PromoCancelling:
		// 回执先部署、取消随后打断：必须已进入回退且任务覆盖已更新组件，
		// 绝不停留在 deploying 的混版状态。
		if dev.State != EnvRollingBack || len(dev.Rollback) == 0 {
			t.Fatalf("cancelling but dev = %s rollback=%d", dev.State, len(dev.Rollback))
		}
	case PromoRunning:
		// 审批+回执赢：order 有一条成功结果，dev 处于 deploying（等待其余组件）。
		if dev.State != EnvDeploying {
			t.Fatalf("running but dev = %s", dev.State)
		}
		if len(dev.Results) != 1 || !dev.Results[0].Success {
			t.Fatalf("dev results = %+v", dev.Results)
		}
	default:
		t.Fatalf("unexpected promotion state %s", got.State())
	}
}

func TestPromotionIdempotencyKeysAndSingleOutbox(t *testing.T) {
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	noGatePolicy(t, svc)

	// 创建晋级的 request id 重放返回同一聚合，不同指纹冲突。
	p1, err := svc.CreatePromotion(tr.ID, "create-p")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.CreatePromotion(tr.ID, "create-p")
	if err != nil || p1.ID != p2.ID {
		t.Fatalf("create replay: %+v %v", p2, err)
	}
	ps, _ := svc.ListPromotions()
	if len(ps) != 1 {
		t.Fatalf("promotions = %d", len(ps))
	}

	l, _ := svc.ClaimDeployment(p1.ID, "dev", "w1", "")
	ov := p1.Snapshot()["order"]
	in := DeployReceiptInput{
		PromotionID: p1.ID, Environment: "dev", Token: l.Token, Attempt: l.Attempt, Epoch: l.Epoch,
		Component: "order", Success: true, Version: &ov, RequestID: "rcpt-order",
	}
	r1, err := svc.ReportDeployment(in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.ReportDeployment(in)
	if err != nil || !r2.Idempotent || r1.Recorded.At != r2.Recorded.At {
		t.Fatalf("receipt retry not replayed: %+v %v", r2, err)
	}
}

func TestPromotionQueryView(t *testing.T) {
	svc := newTestService(t)
	tr := releasedTrain(t, svc)
	threeEnvPolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")

	view := toPromotionDTO(p)
	if len(view.Environments) != 3 {
		t.Fatalf("envs = %d", len(view.Environments))
	}
	dev := view.Environments[0]
	if dev.State != "awaiting_approval" || len(dev.MissingApprovals) != 1 {
		t.Fatalf("dev view = %+v", dev)
	}
	if dev.TargetVersions["payment"] != "1.2.0" {
		t.Fatalf("target versions = %v", dev.TargetVersions)
	}
	if view.BlockingReason == "" {
		t.Fatal("blocking reason should explain pending approvals")
	}
	// 审批依据（策略快照 + 已审批人）随视图返回。
	if len(dev.ApprovalPolicy.Approvers) != 1 {
		t.Fatalf("approval basis missing: %+v", dev.ApprovalPolicy)
	}

	// 失败回退后的视图：执行前版本、组件结果、回退进度、失败原因都可见。
	noGatePolicy(t, svc)
	// 用无门槛策略重建一次晋级以驱动失败场景。
	// （旧晋级先取消。）
	if _, err := svc.CancelPromotion(p.ID, ""); err != nil {
		t.Fatal(err)
	}
	p2, _ := svc.CreatePromotion(tr.ID, "")
	l, _ := svc.ClaimDeployment(p2.ID, "dev", "w1", "")
	ov := p2.Snapshot()["order"]
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p2.ID, Environment: "dev", Token: l.Token, Attempt: 1, Epoch: 1,
		Component: "order", Success: true, Version: &ov,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportDeployment(DeployReceiptInput{
		PromotionID: p2.ID, Environment: "dev", Token: l.Token, Attempt: 1, Epoch: 1,
		Component: "payment", Success: false, Message: "nope",
	}); err != nil {
		t.Fatal(err)
	}
	v2 := toPromotionDTO(mustGetPromo(t, svc, p2.ID))
	dv := v2.Environments[0]
	// order 已更新需回退（1 个任务）；payment 失败但未实际安装、与基线一致，无需恢复。
	if dv.RollbackTotal != 1 || dv.RollbackDone != 0 {
		t.Fatalf("rollback progress = %d/%d", dv.RollbackDone, dv.RollbackTotal)
	}
	if dv.Results[0].Component != "order" || len(dv.Results) != 2 || dv.BeforeVersions != nil {
		t.Fatalf("results/baseline view: %+v", dv)
	}
	if dv.Results[0].FromVersion != "" {
		t.Fatalf("baseline was empty, from should be omitted: %+v", dv.Results)
	}
}

func mustGetPromo(t *testing.T, svc *Service, id string) *Promotion {
	t.Helper()
	p, err := svc.GetPromotion(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPromotionPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	tr := releasedTrain(t, svc)
	threeEnvPolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")
	if _, err := svc.ApprovePromotionEnv(p.ID, "dev", "alice", "qa", ""); err != nil {
		t.Fatal(err)
	}
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w1", "")
	deployAll(t, svc, p.ID, "dev", l)

	// 重启恢复：晋级状态、环境顺序、已晋级环境与 outbox 全部保留。
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(store2)
	got, err := svc2.GetPromotion(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotID != tr.SnapshotID() || len(got.Snapshot()) != 3 {
		t.Fatalf("snapshot restored = %s %v", got.SnapshotID, got.Snapshot())
	}
	if envByName(got, "dev").State != EnvPromoted {
		t.Fatalf("dev restored = %s", envByName(got, "dev").State)
	}
	if envByName(got, "staging").State != EnvAwaitingApproval {
		t.Fatalf("staging restored = %s", envByName(got, "staging").State)
	}
	events, _ := svc2.PendingOutbox()
	n := 0
	for _, e := range events {
		if e.Type == "environment.promoted" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("env events after restart = %d", n)
	}
}

func TestConcurrentDuplicateReceiptsRecordedOnce(t *testing.T) {
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	tr := releasedTrain(t, svc)
	noGatePolicy(t, svc)
	p, _ := svc.CreatePromotion(tr.ID, "")
	l, _ := svc.ClaimDeployment(p.ID, "dev", "w1", "")
	ov := p.Snapshot()["order"]

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.ReportDeployment(DeployReceiptInput{
				PromotionID: p.ID, Environment: "dev", Token: l.Token, Attempt: 1, Epoch: 1,
				Component: "order", Success: true, Version: &ov, RequestID: fmt.Sprintf("r-%d", i),
			})
			// 第一个成功，其余为幂等重放——同租约同组件不允许冲突错误。
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("concurrent receipt: %v", e)
		}
	}
	got, _ := svc.GetPromotion(p.ID)
	if len(envByName(got, "dev").Results) != 1 {
		t.Fatalf("results = %d, want 1", len(envByName(got, "dev").Results))
	}
}
