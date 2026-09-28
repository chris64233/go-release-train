package releasetrain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// registerAndReleaseViaHTTP 完成版本登记、策略、建车、装候选、冻结、审批、放行，返回 trainID。
func registerAndReleaseViaHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/components/order/versions/2.1.0", map[string]any{"constraints": []any{}}},
		{"/components/payment/versions/1.2.0", map[string]any{
			"constraints": []map[string]any{{"component": "order", "op": ">=", "version": "2.0.0"}}}},
		{"/components/gateway/versions/3.0.0", map[string]any{
			"constraints": []map[string]any{
				{"component": "order", "op": ">=", "version": "2.0.0"},
				{"component": "payment", "op": "=", "version": "1.2.0"},
			}}},
	} {
		if w := do(t, h, http.MethodPut, c.path, c.body); w.Code != http.StatusOK {
			t.Fatalf("register %s: %s", c.path, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodPut, "/policy", map[string]any{
		"rules":     []map[string]any{{"role": "qa", "need": 1}},
		"approvers": []map[string]any{{"person": "alice", "role": "qa"}},
	}); w.Code != http.StatusNoContent {
		t.Fatalf("policy: %s", w.Body.String())
	}
	w := do(t, h, http.MethodPost, "/trains", map[string]any{"name": "p"})
	train := decodeBody[trainDTO](t, w)
	for _, c := range []string{"order", "payment", "gateway"} {
		do(t, h, http.MethodPut, "/trains/"+train.ID+"/candidates/"+c, map[string]any{
			"version": map[string]string{"order": "2.1.0", "payment": "1.2.0", "gateway": "3.0.0"}[c],
		})
	}
	if w := do(t, h, http.MethodPost, "/trains/"+train.ID+"/freeze", struct{}{}); w.Code != http.StatusOK {
		t.Fatalf("freeze: %s", w.Body.String())
	}
	do(t, h, http.MethodPost, "/trains/"+train.ID+"/approve", map[string]any{"person": "alice", "role": "qa"})
	if w := do(t, h, http.MethodPost, "/trains/"+train.ID+"/release", struct{}{}); w.Code != http.StatusOK {
		t.Fatalf("release: %s", w.Body.String())
	}
	return train.ID
}

func TestHTTPPromotionFullFlowWithFailureAndRetry(t *testing.T) {
	_, h := newTestServer(t)
	trainID := registerAndReleaseViaHTTP(t, h)

	// 默认环境策略 dev/staging/production（无门槛）。
	w := do(t, h, http.MethodGet, "/environment-policy", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get env policy: %d", w.Code)
	}

	// 未放行校验已通过（这里列车已放行）；创建晋级。
	w = do(t, h, http.MethodPost, "/trains/"+trainID+"/promotions", struct{}{})
	if w.Code != http.StatusCreated {
		t.Fatalf("create promotion: %d %s", w.Code, w.Body.String())
	}
	promo := decodeBody[PromotionView](t, w)
	if len(promo.Environments) != 3 {
		t.Fatalf("envs = %v", promo.Environments)
	}
	pid := promo.ID

	// 领取 dev 部署。
	w = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/claim",
		map[string]any{"worker": "w1"})
	lease := decodeBody[DeploymentLease](t, w)
	if lease.LeaseNo != 1 || len(lease.Targets) != 3 {
		t.Fatalf("lease = %+v", lease)
	}

	// 提交：order/gateway 成功，payment 失败 → rolling_back。
	w = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/receipts", map[string]any{
		"attempt_no": 1, "lease_no": 1,
		"receipts": []map[string]any{
			{"component": "order", "success": true},
			{"component": "gateway", "success": true},
			{"component": "payment", "success": false, "message": "boom"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("receipts: %s", w.Body.String())
	}
	rc := decodeBody[ReceiptOutcome](t, w)
	if rc.Status != "rolling_back" || rc.Promoted {
		t.Fatalf("outcome = %+v", rc)
	}

	// 旧租约号再交回执 → 409 state_conflict（fencing：环境已不在 deploying）。
	w = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/receipts", map[string]any{
		"attempt_no": 1, "lease_no": 1,
		"receipts": []map[string]any{{"component": "order", "success": true}},
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale receipt after rollback status=%d body=%s", w.Code, w.Body.String())
	}

	// 领取并完成两个回退单元。
	for i := 0; i < 2; i++ {
		w = do(t, h, http.MethodPost, "/promotions/"+pid+"/rollback/claim",
			map[string]any{"worker": "rb"})
		if w.Code != http.StatusOK {
			t.Fatalf("rollback claim %d: %s", i, w.Body.String())
		}
		rl := decodeBody[RollbackLease](t, w)
		w = do(t, h, http.MethodPost, "/promotions/"+pid+"/rollback/report", map[string]any{
			"attempt_no": rl.AttemptNo, "lease_no": rl.LeaseNo,
			"environment": rl.Environment, "component": rl.Component, "success": true,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("rollback report: %s", w.Body.String())
		}
	}
	// 再领取回退 → 404（无待处理单元）。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/rollback/claim",
		map[string]any{"worker": "rb"}); w.Code != http.StatusNotFound {
		t.Fatalf("rollback claim when empty = %d", w.Code)
	}

	// 回退完成前不能直接晋级后续环境；现在创建新尝试。
	w = do(t, h, http.MethodPost, "/promotions/"+pid+"/attempts", struct{}{})
	if w.Code != http.StatusCreated {
		t.Fatalf("new attempt: %s", w.Body.String())
	}

	// 三个环境依次领取 + 全成功（dev 需重做）。
	for _, env := range []string{"dev", "staging", "production"} {
		w = do(t, h, http.MethodPost, fmt.Sprintf("/promotions/%s/environments/%s/claim", pid, env),
			map[string]any{"worker": "w2"})
		if w.Code != http.StatusOK {
			t.Fatalf("claim %s: %s", env, w.Body.String())
		}
		l := decodeBody[DeploymentLease](t, w)
		w = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/"+env+"/receipts", map[string]any{
			"attempt_no": l.AttemptNo, "lease_no": l.LeaseNo,
			"receipts": []map[string]any{
				{"component": "order", "success": true},
				{"component": "payment", "success": true},
				{"component": "gateway", "success": true},
			},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("receipts %s: %s", env, w.Body.String())
		}
		o := decodeBody[ReceiptOutcome](t, w)
		if !o.Promoted {
			t.Fatalf("env %s not promoted: %+v", env, o)
		}
	}

	// 查询视图：成功、含两个尝试。
	w = do(t, h, http.MethodGet, "/promotions/"+pid, nil)
	view := decodeBody[PromotionView](t, w)
	if view.State != "succeeded" || len(view.Attempts) != 2 {
		t.Fatalf("final view state=%s attempts=%d", view.State, len(view.Attempts))
	}

	// outbox：3 条 environment.promoted（dev 在第一次尝试失败未出事件）。
	w = do(t, h, http.MethodGet, "/outbox", nil)
	var events []OutboxEvent
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	envCount := 0
	for _, e := range events {
		if e.Type == "environment.promoted" {
			envCount++
		}
	}
	if envCount != 3 {
		t.Fatalf("environment.promoted events = %d, want 3", envCount)
	}
}

func TestHTTPPromotionGateAndOrder(t *testing.T) {
	_, h := newTestServer(t)
	trainID := registerAndReleaseViaHTTP(t, h)

	// 设置带门槛的环境策略：dev 需要 qa。
	w := do(t, h, http.MethodPut, "/environment-policy", map[string]any{
		"environments": []map[string]any{
			{"name": "dev", "rules": []map[string]any{{"role": "qa", "need": 1}},
				"approvers": []map[string]any{{"person": "alice", "role": "qa"}}},
			{"name": "production"},
		},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("put env policy: %s", w.Body.String())
	}

	w = do(t, h, http.MethodPost, "/trains/"+trainID+"/promotions", struct{}{})
	promo := decodeBody[PromotionView](t, w)
	pid := promo.ID

	// 审批未齐领取 dev → 409。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/claim",
		map[string]any{"worker": "w"}); w.Code != http.StatusConflict {
		t.Fatalf("claim dev without approval: %d", w.Code)
	}
	// 越过 dev 领取 production → 409。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/production/claim",
		map[string]any{"worker": "w"}); w.Code != http.StatusConflict {
		t.Fatalf("claim production out of order: %d", w.Code)
	}
	// 无资格审批 → 403。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/approve",
		map[string]any{"environment": "dev", "person": "mallory", "role": "qa"}); w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized env approval: %d", w.Code)
	}
	// 合法审批后 dev 可领取。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/approve",
		map[string]any{"environment": "dev", "person": "alice", "role": "qa"}); w.Code != http.StatusOK {
		t.Fatalf("approve dev: %s", w.Body.String())
	}
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/claim",
		map[string]any{"worker": "w"}); w.Code != http.StatusOK {
		t.Fatalf("claim dev after approval: %s", w.Body.String())
	}
}

func TestHTTPPromotionNotFoundAndBadInput(t *testing.T) {
	_, h := newTestServer(t)
	if w := do(t, h, http.MethodGet, "/promotions/promo_nope", nil); w.Code != http.StatusNotFound {
		t.Fatalf("get missing promotion: %d", w.Code)
	}
	// 对不存在列车创建晋级 → 404。
	if w := do(t, h, http.MethodPost, "/trains/train_nope/promotions", struct{}{}); w.Code != http.StatusNotFound {
		t.Fatalf("promotion on missing train: %d", w.Code)
	}
	// 回执缺少 attempt/lease → 400。
	if w := do(t, h, http.MethodPost, "/promotions/promo_x/environments/dev/receipts", map[string]any{
		"receipts": []map[string]any{{"component": "a", "success": true}},
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("receipts without lease: %d", w.Code)
	}
}
