package releasetrain

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*Service, http.Handler) {
	t.Helper()
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store)
	return svc, NewHandler(svc)
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func TestHTTPFullFlow(t *testing.T) {
	svc, h := newTestServer(t)
	_ = svc

	// 登记版本
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
		w := do(t, h, http.MethodPut, c.path, c.body)
		if w.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", c.path, w.Code, w.Body.String())
		}
	}

	// 设置策略
	w := do(t, h, http.MethodPut, "/policy", map[string]any{
		"rules":     []map[string]any{{"role": "qa", "need": 1}, {"role": "manager", "need": 1}},
		"approvers": []map[string]any{{"person": "alice", "role": "qa"}, {"person": "bob", "role": "manager"}},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("put policy: %d %s", w.Code, w.Body.String())
	}

	// 创建列车
	w = do(t, h, http.MethodPost, "/trains", map[string]any{"name": "flow"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	train := decodeBody[trainDTO](t, w)

	// 加候选
	for _, c := range []struct{ name, ver string }{
		{"order", "2.1.0"}, {"payment", "1.2.0"}, {"gateway", "3.0.0"},
	} {
		w = do(t, h, http.MethodPut, "/trains/"+train.ID+"/candidates/"+c.name,
			map[string]any{"version": c.ver})
		if w.Code != http.StatusOK {
			t.Fatalf("candidate %s: %d %s", c.name, w.Code, w.Body.String())
		}
	}

	// 冻结
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/freeze", struct{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("freeze: %d %s", w.Code, w.Body.String())
	}
	frozen := decodeBody[trainDTO](t, w)
	if frozen.State != "frozen" || len(frozen.FrozenSnapshot) != 3 {
		t.Fatalf("frozen = %+v", frozen)
	}

	// 审批：无资格人员 403
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/approve",
		map[string]any{"person": "mallory", "role": "qa"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized approve: %d", w.Code)
	}
	// 正式审批
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/approve",
		map[string]any{"person": "alice", "role": "qa"})
	if w.Code != http.StatusOK {
		t.Fatalf("approve qa: %s", w.Body.String())
	}
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/approve",
		map[string]any{"person": "bob", "role": "manager"})
	if w.Code != http.StatusOK {
		t.Fatalf("approve manager: %s", w.Body.String())
	}

	// 放行，重复放行幂等
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/release", struct{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	first := decodeBody[ReleaseResult](t, w)
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/release", struct{}{})
	second := decodeBody[ReleaseResult](t, w)
	if first.EventID != second.EventID {
		t.Fatal("repeat release changed event id")
	}

	// 终态取消 409
	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/cancel", struct{}{})
	if w.Code != http.StatusConflict {
		t.Fatalf("cancel after release: %d", w.Code)
	}

	// outbox 恰有一条
	w = do(t, h, http.MethodGet, "/outbox", nil)
	events := decodeBody[[]OutboxEvent](t, w)
	if len(events) != 1 || events[0].ID != first.EventID {
		t.Fatalf("outbox = %+v", events)
	}
}

func TestHTTPFreezeDependencyErrorCarriesProblems(t *testing.T) {
	svc, h := newTestServer(t)
	// order 1.0.0 与 payment 1.0.0（要求 order >=2.0.0）不兼容。
	do(t, h, http.MethodPut, "/components/order/versions/1.0.0", map[string]any{"constraints": []any{}})
	do(t, h, http.MethodPut, "/components/payment/versions/1.0.0", map[string]any{
		"constraints": []map[string]any{{"component": "order", "op": ">=", "version": "2.0.0"}}})
	if err := svc.PutPolicy(
		[]ApprovalRule{{Role: "qa", Need: 1}},
		[]Approver{{Person: "alice", Role: "qa"}}); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, http.MethodPost, "/trains", map[string]any{"name": "bad"})
	train := decodeBody[trainDTO](t, w)
	do(t, h, http.MethodPut, "/trains/"+train.ID+"/candidates/order", map[string]any{"version": "1.0.0"})
	do(t, h, http.MethodPut, "/trains/"+train.ID+"/candidates/payment", map[string]any{"version": "1.0.0"})

	w = do(t, h, http.MethodPost, "/trains/"+train.ID+"/freeze", struct{}{})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	var body struct {
		Code     string   `json:"code"`
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "dependency" || len(body.Problems) == 0 {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestHTTPOptimisticConflictStatus(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, http.MethodPut, "/components/order/versions/1.0.0", map[string]any{"constraints": []any{}})
	w := do(t, h, http.MethodPost, "/trains", map[string]any{"name": "t"})
	train := decodeBody[trainDTO](t, w)

	// 用错误的 expected_version 更新 -> 409 version_conflict。
	w = do(t, h, http.MethodPut, "/trains/"+train.ID+"/candidates/order",
		map[string]any{"version": "1.0.0", "expected_version": 999})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "version_conflict") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHTTPReadEndpoints(t *testing.T) {
	svc, h := newTestServer(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)

	if w := do(t, h, http.MethodGet, "/health", nil); w.Code != http.StatusOK {
		t.Fatalf("health: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/policy", nil); w.Code != http.StatusOK {
		t.Fatalf("get policy: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/components/payment/versions/1.2.0", nil); w.Code != http.StatusOK {
		t.Fatalf("get version: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/components/payment/versions/9.9.9", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing version: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/versions?component=payment", nil); w.Code != http.StatusOK {
		t.Fatalf("list versions: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/versions", nil); w.Code != http.StatusOK {
		t.Fatalf("list all: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/versions?component=ghost", nil); w.Code != http.StatusNotFound {
		t.Fatalf("list ghost: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/trains", nil); w.Code != http.StatusOK {
		t.Fatalf("list trains: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/trains/"+tr.ID, nil); w.Code != http.StatusOK {
		t.Fatalf("get train: %d", w.Code)
	}
	if w := do(t, h, http.MethodDelete, "/trains/"+tr.ID+"/candidates/gateway", nil); w.Code != http.StatusOK {
		t.Fatalf("delete candidate: %d", w.Code)
	}

	// 放行后标记 outbox 已派发。
	freezeAndApprove(t, svc, tr.ID)
	rel := decodeBody[ReleaseResult](t, do(t, h, http.MethodPost, "/trains/"+tr.ID+"/release", struct{}{}))
	if w := do(t, h, http.MethodPost, "/outbox/"+rel.EventID+"/dispatch", nil); w.Code != http.StatusNoContent {
		t.Fatalf("dispatch: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/outbox/evt_nope/dispatch", nil); w.Code != http.StatusNotFound {
		t.Fatalf("dispatch missing: %d", w.Code)
	}
}

func TestHTTPMalformedJSON(t *testing.T) {
	_, h := newTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/trains", bytes.NewReader([]byte("{not json")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed json: %d", w.Code)
	}
}

func TestHTTPNotFoundAndBadRequest(t *testing.T) {
	_, h := newTestServer(t)
	if w := do(t, h, http.MethodGet, "/trains/train_nope", nil); w.Code != http.StatusNotFound {
		t.Fatalf("get missing train: %d", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/components/order/versions/not-a-version",
		map[string]any{}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad version: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/trains",
		map[string]any{"name": "x", "junk": 1}); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field should be rejected, status=%d", w.Code)
	}
}
