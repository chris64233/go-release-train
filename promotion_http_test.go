package releasetrain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// setupReleasedTrainHTTP 准备一列车并完成登记/冻结/审批/放行，返回放行的列车 ID。
func setupReleasedTrainHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	regs := []struct {
		path string
		body map[string]any
	}{
		{"/components/order/versions/2.1.0", map[string]any{"constraints": []any{}}},
		{"/components/payment/versions/1.2.0", map[string]any{
			"constraints": []map[string]any{{"component": "order", "op": ">=", "version": "2.0.0"}}}},
	}
	for _, c := range regs {
		if w := do(t, h, http.MethodPut, c.path, c.body); w.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", c.path, w.Code, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodPut, "/policy", map[string]any{
		"rules":     []map[string]any{{"role": "qa", "need": 1}},
		"approvers": []map[string]any{{"person": "alice", "role": "qa"}},
	}); w.Code != http.StatusNoContent {
		t.Fatalf("policy: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodPost, "/trains", map[string]any{"name": "R1"})
	tr := decodeBody[trainDTO](t, w)
	for _, c := range []struct {
		name string
		ver  string
	}{{"order", "2.1.0"}, {"payment", "1.2.0"}} {
		if w := do(t, h, http.MethodPut, "/trains/"+tr.ID+"/candidates/"+c.name,
			map[string]any{"version": c.ver}); w.Code != http.StatusOK {
			t.Fatalf("candidate: %d %s", w.Code, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodPost, "/trains/"+tr.ID+"/freeze", map[string]any{}); w.Code != http.StatusOK {
		t.Fatalf("freeze: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPost, "/trains/"+tr.ID+"/approve",
		map[string]any{"person": "alice", "role": "qa"}); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPost, "/trains/"+tr.ID+"/release", map[string]any{}); w.Code != http.StatusOK {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	return tr.ID
}

func TestHTTPPromotionFlow(t *testing.T) {
	_, h := newTestServer(t)
	trainID := setupReleasedTrainHTTP(t, h)

	// 未放行校验：先在没有晋级策略时创建应 400（设置前）。
	// 设置环境晋级策略 dev -> prod（dev 需 qa 审批，prod 无门槛）。
	if w := do(t, h, http.MethodPut, "/promotion-policy", map[string]any{
		"environments": []map[string]any{
			{
				"name":      "dev",
				"rules":     []map[string]any{{"role": "qa", "need": 1}},
				"approvers": []map[string]any{{"person": "alice", "role": "qa"}},
			},
			{"name": "prod"},
		},
	}); w.Code != http.StatusNoContent {
		t.Fatalf("promotion policy: %d %s", w.Code, w.Body.String())
	}

	w := do(t, h, http.MethodPost, "/promotions", map[string]any{"train_id": trainID})
	if w.Code != http.StatusCreated {
		t.Fatalf("create promotion: %d %s", w.Code, w.Body.String())
	}
	promo := decodeBody[promotionDTO](t, w)
	if len(promo.Environments) != 2 {
		t.Fatalf("envs = %d", len(promo.Environments))
	}
	pid := promo.ID

	// 越级操作 prod：state_conflict（409）。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/prod/deploy/claim",
		map[string]any{"worker": "w"}); w.Code != http.StatusConflict {
		t.Fatalf("claim prod early: %d", w.Code)
	}

	// dev 审批 -> 领取部署。
	if w := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/approve",
		map[string]any{"person": "alice", "role": "qa"}); w.Code != http.StatusOK {
		t.Fatalf("approve dev: %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/deploy/claim",
		map[string]any{"worker": "w1"})
	if w.Code != http.StatusOK {
		t.Fatalf("claim dev: %d %s", w.Code, w.Body.String())
	}
	lease := decodeBody[DeployLease](t, w)

	// order 成功，payment 失败 -> 回退。
	report := func(env string, l DeployLease, comp string, success bool, ver string) *httptest.ResponseRecorder {
		body := map[string]any{
			"token": l.Token, "attempt": l.Attempt, "epoch": l.Epoch,
			"component": comp, "success": success,
		}
		if ver != "" {
			body["version"] = ver
		}
		return do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/"+env+"/deploy/report", body)
	}
	if rw := report("dev", lease, "order", true, "2.1.0"); rw.Code != http.StatusOK {
		t.Fatalf("report order: %d %s", rw.Code, rw.Body.String())
	}
	if rw := report("dev", lease, "payment", false, ""); rw.Code != http.StatusOK {
		t.Fatalf("report payment fail: %d %s", rw.Code, rw.Body.String())
	}

	// 查询：dev rolling_back，回退 1 个任务（order），有阻断原因。
	p := decodeBody[promotionDTO](t, do(t, h, http.MethodGet, "/promotions/"+pid, nil))
	if p.Environments[0].State != "rolling_back" {
		t.Fatalf("dev state = %s", p.Environments[0].State)
	}
	if p.Environments[0].RollbackTotal != 1 {
		t.Fatalf("rollback total = %d", p.Environments[0].RollbackTotal)
	}
	if p.BlockingReason == "" {
		t.Fatal("missing blocking reason")
	}

	// 旧部署租约回退回执 -> 409 lease_conflict。
	rbBody := map[string]any{
		"token": lease.Token, "attempt": lease.Attempt, "epoch": lease.Epoch,
		"component": "order", "status": "succeeded",
	}
	if rw := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/rollback/report", rbBody); rw.Code != http.StatusConflict {
		t.Fatalf("rollback with deploy lease: %d", rw.Code)
	}

	// 领取回退 -> order 恢复成功 -> failed。
	rw := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/rollback/claim",
		map[string]any{"worker": "rb"})
	rb := decodeBody[RollbackLease](t, rw)
	if len(rb.Tasks) != 1 || rb.Tasks[0].Component != "order" {
		t.Fatalf("rollback tasks = %+v", rb.Tasks)
	}
	rbBody = map[string]any{
		"token": rb.Token, "attempt": rb.Attempt, "epoch": rb.Epoch,
		"component": "order", "status": "succeeded",
	}
	if rw := do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/rollback/report", rbBody); rw.Code != http.StatusOK {
		t.Fatalf("rollback report: %d %s", rw.Code, rw.Body.String())
	}
	p = decodeBody[promotionDTO](t, do(t, h, http.MethodGet, "/promotions/"+pid, nil))
	if p.State != "failed" {
		t.Fatalf("promo state = %s, want failed", p.State)
	}

	// 重试：新尝试沿用快照。
	if rw := do(t, h, http.MethodPost, "/promotions/"+pid+"/retry", map[string]any{}); rw.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rw.Code, rw.Body.String())
	}
	// dev 需重新审批。
	do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/approve",
		map[string]any{"person": "alice", "role": "qa"})
	rw = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/dev/deploy/claim",
		map[string]any{"worker": "w2"})
	l2 := decodeBody[DeployLease](t, rw)
	if l2.Attempt != 2 {
		t.Fatalf("attempt = %d", l2.Attempt)
	}
	if rw := report("dev", l2, "order", true, "2.1.0"); rw.Code != http.StatusOK {
		t.Fatalf("retry deploy order: %s", rw.Body.String())
	}
	if rw := report("dev", l2, "payment", true, "1.2.0"); rw.Code != http.StatusOK {
		t.Fatalf("retry deploy payment: %s", rw.Body.String())
	}

	// prod 无门槛：领取并全部部署。
	rw = do(t, h, http.MethodPost, "/promotions/"+pid+"/environments/prod/deploy/claim",
		map[string]any{"worker": "w3"})
	l3 := decodeBody[DeployLease](t, rw)
	if rw := report("prod", l3, "order", true, "2.1.0"); rw.Code != http.StatusOK {
		t.Fatalf("prod order: %s", rw.Body.String())
	}
	if rw := report("prod", l3, "payment", true, "1.2.0"); rw.Code != http.StatusOK {
		t.Fatalf("prod payment: %s", rw.Body.String())
	}

	p = decodeBody[promotionDTO](t, do(t, h, http.MethodGet, "/promotions/"+pid, nil))
	if p.State != "promoted" {
		t.Fatalf("final state = %s", p.State)
	}
	for _, e := range p.Environments {
		if e.State != "promoted" || e.EventID == "" {
			t.Fatalf("env %s not promoted: %+v", e.Name, e)
		}
	}

	// outbox：两个 environment.promoted 事件 + 一条 train.released。
	events := decodeBody[[]OutboxEvent](t, do(t, h, http.MethodGet, "/outbox", nil))
	envEvents, trainEvents := 0, 0
	for _, e := range events {
		switch e.Type {
		case "environment.promoted":
			envEvents++
		case "train.released":
			trainEvents++
		}
	}
	if envEvents != 2 || trainEvents != 1 {
		t.Fatalf("outbox env=%d train=%d", envEvents, trainEvents)
	}
}

func TestHTTPPromotionNotReleasedRejected(t *testing.T) {
	_, h := newTestServer(t)
	// 只登记 + 建车，不冻结放行。
	do(t, h, http.MethodPut, "/components/order/versions/2.1.0", map[string]any{"constraints": []any{}})
	do(t, h, http.MethodPut, "/promotion-policy", map[string]any{
		"environments": []map[string]any{{"name": "dev"}},
	})
	tr := decodeBody[trainDTO](t, do(t, h, http.MethodPost, "/trains", map[string]any{"name": "x"}))
	w := do(t, h, http.MethodPost, "/promotions", map[string]any{"train_id": tr.ID})
	if w.Code != http.StatusConflict {
		t.Fatalf("promote editing train: code=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &body)
}
