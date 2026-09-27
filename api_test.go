package releasetrain

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type apiHarness struct {
	t   *testing.T
	svc *Service
	srv *Server
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	svc, err := NewService(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	return &apiHarness{t: t, svc: svc, srv: NewServer(svc)}
}

func (h *apiHarness) do(method, path string, body any, idemKey string, headers map[string]string) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func decodeBody[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode response %s: %v", string(b), err)
	}
	return v
}

// TestAPIFullFlow 端到端走通：登记 → 策略 → 建车 → 冻结 → 审批 → 放行 → 重复放行。
func TestAPIFullFlow(t *testing.T) {
	h := newAPIHarness(t)

	mustStatus := func(got, want int, body []byte, ctx string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: status = %d, want %d, body: %s", ctx, got, want, string(body))
		}
	}

	// 登记组件与版本。
	for _, name := range []string{"api", "lib"} {
		st, _ := h.do(http.MethodPost, "/v1/components", map[string]string{"name": name}, "comp-"+name, nil)
		mustStatus(st, http.StatusCreated, nil, "register component "+name)
	}
	st, body := h.do(http.MethodPost, "/v1/components/lib/versions",
		map[string]any{"version": "1.0.0", "dependencies": []any{}}, "v-lib-1", nil)
	mustStatus(st, http.StatusCreated, body, "register lib 1.0.0")
	st, body = h.do(http.MethodPost, "/v1/components/api/versions", map[string]any{
		"version": "1.0.0",
		"dependencies": []map[string]string{
			{"component": "lib", "constraint": "^1.0.0"},
		},
	}, "v-api-1", nil)
	mustStatus(st, http.StatusCreated, body, "register api 1.0.0")

	// 冻结时的策略。
	policy := map[string]any{"rules": []map[string]any{
		{"role": "qa", "members": []string{"alice"}, "threshold": 1},
	}}
	st, body = h.do(http.MethodPut, "/v1/policy", policy, "", nil)
	mustStatus(st, http.StatusOK, body, "set policy")

	// 建车。
	st, body = h.do(http.MethodPost, "/v1/trains", map[string]any{
		"id": "T1",
		"candidates": []map[string]string{
			{"component": "api", "version": "1.0.0"},
			{"component": "lib", "version": "1.0.0"},
		},
	}, "train-T1", nil)
	mustStatus(st, http.StatusCreated, body, "create train")
	train := decodeBody[Train](t, body)

	// 冻结，带修订号条件。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/freeze", nil, "freeze-T1",
		map[string]string{"X-Expected-Revision": "1"})
	mustStatus(st, http.StatusOK, body, "freeze")
	if got := decodeBody[Train](t, body); got.State != StateFrozen {
		t.Fatalf("frozen train: %+v", got)
	}

	// 错误修订号冻结 -> 412，且响应带当前修订号。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/freeze", nil, "freeze-T1-stale",
		map[string]string{"X-Expected-Revision": "1"})
	if st != http.StatusPreconditionFailed {
		t.Fatalf("stale freeze status = %d, want 412: %s", st, body)
	}
	errBody := decodeBody[struct {
		Error struct {
			Kind    string `json:"kind"`
			Current int64  `json:"current_revision"`
		} `json:"error"`
	}](t, body)
	if errBody.Error.Kind != KindVersion || errBody.Error.Current != 2 {
		t.Fatalf("unexpected error body: %+v", errBody)
	}

	// 无资格人员审批 -> 403。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/approvals",
		map[string]any{"person": "mallory", "role": "qa", "expected_revision": 2}, "ap-bad", nil)
	if st != http.StatusForbidden {
		t.Fatalf("unauthorized approval status = %d: %s", st, body)
	}

	// 合法审批。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/approvals",
		map[string]any{"person": "alice", "role": "qa", "expected_revision": 2}, "ap-alice-1", nil)
	mustStatus(st, http.StatusOK, body, "approve alice")

	// 放行：当前修订号为 3。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/release", nil, "rel-T1-1",
		map[string]string{"X-Expected-Revision": "3"})
	mustStatus(st, http.StatusOK, body, "release")
	first := decodeBody[ReleaseResult](t, body)
	if first.OutboxID != 1 {
		t.Fatalf("release result: %+v", first)
	}

	// 重复放行（不同幂等键、过期修订号）：返回原结果。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/release", nil, "rel-T1-2",
		map[string]string{"X-Expected-Revision": "3"})
	mustStatus(st, http.StatusOK, body, "duplicate release")
	second := decodeBody[ReleaseResult](t, body)
	if second.OutboxID != first.OutboxID || second.Revision != first.Revision {
		t.Fatalf("duplicate release differs: %+v vs %+v", first, second)
	}

	// 已放行再取消 -> 409。
	st, body = h.do(http.MethodPost, "/v1/trains/T1/cancel",
		map[string]any{"reason": "late", "expected_revision": 4}, "cancel-T1", nil)
	if st != http.StatusConflict {
		t.Fatalf("cancel released train status = %d: %s", st, body)
	}

	// outbox 仅一条。
	st, body = h.do(http.MethodGet, "/v1/outbox", nil, "", nil)
	mustStatus(st, http.StatusOK, body, "list outbox")
	ob := decodeBody[struct {
		Events []OutboxEvent `json:"events"`
	}](t, body)
	if len(ob.Events) != 1 || ob.Events[0].Type != "train.released" {
		t.Fatalf("outbox: %+v", ob.Events)
	}

	_ = train
}

// TestAPIIdempotencyKeyReplay 同一键同体重放返回原响应；不同体冲突。
func TestAPIIdempotencyKeyReplay(t *testing.T) {
	h := newAPIHarness(t)

	st1, b1 := h.do(http.MethodPost, "/v1/components", map[string]string{"name": "lib"}, "key-1", nil)
	if st1 != http.StatusCreated {
		t.Fatalf("first: %d %s", st1, b1)
	}
	st2, b2 := h.do(http.MethodPost, "/v1/components", map[string]string{"name": "lib"}, "key-1", nil)
	if st2 != st1 || !bytes.Equal(b1, b2) {
		t.Fatalf("replay mismatch: %d/%s vs %d/%s", st1, b1, st2, b2)
	}

	// 同键不同体 -> 409 幂等冲突。
	st3, b3 := h.do(http.MethodPost, "/v1/components", map[string]string{"name": "other"}, "key-1", nil)
	if st3 != http.StatusConflict {
		t.Fatalf("idempotency conflict status = %d: %s", st3, b3)
	}
	eb := decodeBody[struct {
		Error struct{ Kind string } `json:"error"`
	}](t, b3)
	if eb.Error.Kind != KindIdempotency {
		t.Fatalf("error kind = %q", eb.Error.Kind)
	}
}

// TestAPIFreezeDependencyFailure 依赖不满足时冻结返回 422，列车保持 open。
func TestAPIFreezeDependencyFailure(t *testing.T) {
	h := newAPIHarness(t)
	h.do(http.MethodPost, "/v1/components", map[string]string{"name": "api"}, "c1", nil)
	h.do(http.MethodPost, "/v1/components", map[string]string{"name": "lib"}, "c2", nil)
	h.do(http.MethodPost, "/v1/components/api/versions", map[string]any{
		"version": "1.0.0",
		"dependencies": []map[string]string{
			{"component": "lib", "constraint": "^2.0.0"},
		},
	}, "v1", nil)
	h.do(http.MethodPost, "/v1/components/lib/versions", map[string]any{"version": "1.0.0"}, "v2", nil)

	h.do(http.MethodPost, "/v1/trains", map[string]any{
		"id": "T1",
		"candidates": []map[string]string{
			{"component": "api", "version": "1.0.0"},
			{"component": "lib", "version": "1.0.0"},
		},
	}, "t1", nil)

	st, body := h.do(http.MethodPost, "/v1/trains/T1/freeze", nil, "f1",
		map[string]string{"X-Expected-Revision": "1"})
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("freeze conflict status = %d: %s", st, body)
	}
	eb := decodeBody[struct {
		Error struct{ Kind string } `json:"error"`
	}](t, body)
	if eb.Error.Kind != KindDependency {
		t.Fatalf("kind = %q, want dependency", eb.Error.Kind)
	}

	st, body = h.do(http.MethodGet, "/v1/trains/T1", nil, "", nil)
	if st != http.StatusOK {
		t.Fatalf("get train: %d %s", st, body)
	}
	if got := decodeBody[Train](t, body); got.State != StateOpen || got.Revision != 1 {
		t.Fatalf("train after failed freeze: %+v", got)
	}
}

// TestAPIReleaseBeforeApproval 审批未齐放行返回 403。
func TestAPIReleaseBeforeApproval(t *testing.T) {
	h := newAPIHarness(t)
	h.do(http.MethodPost, "/v1/components", map[string]string{"name": "lib"}, "c", nil)
	h.do(http.MethodPost, "/v1/components/lib/versions", map[string]any{"version": "1.0.0"}, "v", nil)
	h.do(http.MethodPut, "/v1/policy", map[string]any{"rules": []map[string]any{
		{"role": "qa", "members": []string{"alice"}, "threshold": 1},
	}}, "", nil)
	h.do(http.MethodPost, "/v1/trains", map[string]any{
		"id":         "T1",
		"candidates": []map[string]string{{"component": "lib", "version": "1.0.0"}},
	}, "t", nil)
	h.do(http.MethodPost, "/v1/trains/T1/freeze", nil, "f",
		map[string]string{"X-Expected-Revision": "1"})

	st, body := h.do(http.MethodPost, "/v1/trains/T1/release", nil, "r",
		map[string]string{"X-Expected-Revision": "2"})
	if st != http.StatusForbidden {
		t.Fatalf("release without approval status = %d: %s", st, body)
	}
}

// TestAPIInvalidJSON 非法 JSON 返回 400 而非 500。
func TestAPIInvalidJSON(t *testing.T) {
	h := newAPIHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/components", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
