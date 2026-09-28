package releasetrain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Handler 把 Service 暴露为 JSON HTTP 接口。
type Handler struct {
	svc *Service
	mux *http.ServeMux
}

// NewHandler 构建路由。
func NewHandler(svc *Service) *Handler {
	h := &Handler{svc: svc, mux: http.NewServeMux()}
	h.routes()
	return h
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) routes() {
	m := h.mux
	m.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	m.HandleFunc("PUT /policy", h.putPolicy)
	m.HandleFunc("GET /policy", h.getPolicy)

	m.HandleFunc("PUT /components/{component}/versions/{version}", h.registerVersion)
	m.HandleFunc("GET /components/{component}/versions/{version}", h.getVersion)
	m.HandleFunc("GET /versions", h.listVersions)

	m.HandleFunc("POST /trains", h.createTrain)
	m.HandleFunc("GET /trains", h.listTrains)
	m.HandleFunc("GET /trains/{id}", h.getTrain)
	m.HandleFunc("PUT /trains/{id}/candidates/{component}", h.setCandidate)
	m.HandleFunc("DELETE /trains/{id}/candidates/{component}", h.removeCandidate)
	m.HandleFunc("POST /trains/{id}/freeze", h.freeze)
	m.HandleFunc("POST /trains/{id}/approve", h.approve)
	m.HandleFunc("POST /trains/{id}/cancel", h.cancel)
	m.HandleFunc("POST /trains/{id}/release", h.release)

	m.HandleFunc("GET /outbox", h.listOutbox)
	m.HandleFunc("POST /outbox/{id}/dispatch", h.dispatchOutbox)

	// 跨环境逐级晋级
	m.HandleFunc("PUT /promotion-policy", h.putPromotionPolicy)
	m.HandleFunc("GET /promotion-policy", h.getPromotionPolicy)
	m.HandleFunc("POST /promotions", h.createPromotion)
	m.HandleFunc("GET /promotions", h.listPromotions)
	m.HandleFunc("GET /promotions/{pid}", h.getPromotion)
	m.HandleFunc("POST /promotions/{pid}/cancel", h.cancelPromotion)
	m.HandleFunc("POST /promotions/{pid}/retry", h.retryPromotion)
	m.HandleFunc("POST /promotions/{pid}/environments/{env}/approve", h.approveEnv)
	m.HandleFunc("POST /promotions/{pid}/environments/{env}/deploy/claim", h.claimDeploy)
	m.HandleFunc("POST /promotions/{pid}/environments/{env}/deploy/report", h.reportDeploy)
	m.HandleFunc("POST /promotions/{pid}/environments/{env}/rollback/claim", h.claimRollback)
	m.HandleFunc("POST /promotions/{pid}/environments/{env}/rollback/report", h.reportRollback)
}

// ---- 请求/响应 DTO ----

type constraintDTO struct {
	Component string `json:"component"`
	Op        string `json:"op"`
	Version   string `json:"version"`
}

type registerVersionReq struct {
	Constraints []constraintDTO `json:"constraints"`
	RequestID   string          `json:"request_id"`
}

type policyReq struct {
	Rules     []ApprovalRule `json:"rules"`
	Approvers []Approver     `json:"approvers"`
}

type createTrainReq struct {
	Name      string `json:"name"`
	RequestID string `json:"request_id"`
}

type setCandidateReq struct {
	Version         string `json:"version"`
	ExpectedVersion int    `json:"expected_version"`
	RequestID       string `json:"request_id"`
}

type removeCandidateReq struct {
	ExpectedVersion int    `json:"expected_version"`
	RequestID       string `json:"request_id"`
}

type trainActionReq struct {
	Person          string `json:"person,omitempty"`
	Role            string `json:"role,omitempty"`
	ExpectedVersion int    `json:"expected_version"`
	RequestID       string `json:"request_id"`
}

// trainDTO 是列车的对外 JSON 形态（聚合的 state 等私有字段在此显式暴露）。
type trainDTO struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	State          string            `json:"state"`
	Version        int               `json:"version"`
	Candidates     map[string]string `json:"candidates"`
	FrozenSnapshot map[string]string `json:"frozen_snapshot,omitempty"`
	SnapshotID     string            `json:"snapshot_id,omitempty"`
	Policy         *PolicySnapshot   `json:"policy,omitempty"`
	Approvals      []Approval        `json:"approvals,omitempty"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	FrozenAt       string            `json:"frozen_at,omitempty"`
	ReleasedAt     string            `json:"released_at,omitempty"`
	CancelledAt    string            `json:"cancelled_at,omitempty"`
	ReleaseEventID string            `json:"release_event_id,omitempty"`
}

func toTrainDTO(t *Train) trainDTO {
	dto := trainDTO{
		ID:             t.ID,
		Name:           t.Name,
		State:          string(t.State()),
		Version:        t.Version,
		Candidates:     versionsToStrings(t.Candidates()),
		FrozenSnapshot: versionsToStrings(t.Snapshot()),
		SnapshotID:     t.SnapshotID(),
		Policy:         t.Policy(),
		Approvals:      t.Approvals(),
		ReleaseEventID: t.ReleaseEventID(),
	}
	if !t.CreatedAt().IsZero() {
		dto.CreatedAt = t.CreatedAt().Format(timeRFC3339)
	}
	if !t.UpdatedAt().IsZero() {
		dto.UpdatedAt = t.UpdatedAt().Format(timeRFC3339)
	}
	if at := t.FrozenAt(); !at.IsZero() {
		dto.FrozenAt = at.Format(timeRFC3339)
	}
	if at := t.ReleasedAt(); !at.IsZero() {
		dto.ReleasedAt = at.Format(timeRFC3339)
	}
	if at := t.CancelledAt(); !at.IsZero() {
		dto.CancelledAt = at.Format(timeRFC3339)
	}
	return dto
}

func versionsToStrings(in map[string]Version) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v.String()
	}
	return out
}

// ---- 处理器 ----

func (h *Handler) registerVersion(w http.ResponseWriter, r *http.Request) {
	component := r.PathValue("component")
	v, err := ParseVersion(r.PathValue("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	var req registerVersionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	cs := make([]Constraint, 0, len(req.Constraints))
	for _, c := range req.Constraints {
		cv, err := ParseVersion(c.Version)
		if err != nil {
			writeError(w, err)
			return
		}
		cs = append(cs, Constraint{Component: c.Component, Op: ConstraintOp(c.Op), Version: cv})
	}
	rec, err := h.svc.RegisterVersion(RegisterVersionInput{
		Component: component, Version: v, Constraints: cs, RequestID: strings.TrimSpace(req.RequestID),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) getVersion(w http.ResponseWriter, r *http.Request) {
	v, err := ParseVersion(r.PathValue("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	rec, err := h.svc.GetVersion(r.PathValue("component"), v)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) listVersions(w http.ResponseWriter, r *http.Request) {
	recs, err := h.svc.ListVersions(strings.TrimSpace(r.URL.Query().Get("component")))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (h *Handler) putPolicy(w http.ResponseWriter, r *http.Request) {
	var req policyReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := h.svc.PutPolicy(req.Rules, req.Approvers); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	p := h.svc.GetPolicy()
	if p == nil {
		writeJSON(w, http.StatusOK, PolicySnapshot{Rules: []ApprovalRule{}, Approvers: []Approver{}})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) createTrain(w http.ResponseWriter, r *http.Request) {
	var req createTrainReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := h.svc.CreateTrain(req.Name, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTrainDTO(t))
}

func (h *Handler) listTrains(w http.ResponseWriter, _ *http.Request) {
	ts, err := h.svc.ListTrains()
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]trainDTO, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTrainDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getTrain(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTrain(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrainDTO(t))
}

func (h *Handler) setCandidate(w http.ResponseWriter, r *http.Request) {
	var req setCandidateReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	v, err := ParseVersion(req.Version)
	if err != nil {
		writeError(w, err)
		return
	}
	t, err := h.svc.SetCandidate(SetCandidateInput{
		TrainID:         r.PathValue("id"),
		Component:       r.PathValue("component"),
		Version:         v,
		ExpectedVersion: req.ExpectedVersion,
		RequestID:       strings.TrimSpace(req.RequestID),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrainDTO(t))
}

func (h *Handler) removeCandidate(w http.ResponseWriter, r *http.Request) {
	var req removeCandidateReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := h.svc.RemoveCandidate(r.PathValue("id"), r.PathValue("component"),
		req.ExpectedVersion, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrainDTO(t))
}

func (h *Handler) freeze(w http.ResponseWriter, r *http.Request) {
	var req trainActionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := h.svc.Freeze(r.PathValue("id"), req.ExpectedVersion, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrainDTO(t))
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	var req trainActionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := h.svc.Approve(r.PathValue("id"), req.Person, req.Role,
		req.ExpectedVersion, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrainDTO(t))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var req trainActionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := h.svc.Cancel(r.PathValue("id"), req.ExpectedVersion, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrainDTO(t))
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	var req trainActionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := h.svc.Release(r.PathValue("id"), req.ExpectedVersion, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) listOutbox(w http.ResponseWriter, _ *http.Request) {
	events, err := h.svc.PendingOutbox()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (h *Handler) dispatchOutbox(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.MarkOutboxDispatched(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 基础设施 ----

const timeRFC3339 = "2006-01-02T15:04:05.000Z07:00"

func decode(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return errBadJSON(err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil // 允许空请求体，全部字段取零值
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadJSON(err)
	}
	return nil
}

func errBadJSON(err error) error {
	return fmt.Errorf("%w: malformed JSON body: %v", ErrInvalidArgument, err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusFor 把领域错误类别映射为 HTTP 状态码与机器可读错误码。
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrDependency):
		return http.StatusUnprocessableEntity, "dependency"
	case errors.Is(err, ErrApproval):
		return http.StatusForbidden, "approval"
	case errors.Is(err, ErrVersionConflict):
		return http.StatusConflict, "version_conflict"
	case errors.Is(err, ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, ErrStateConflict):
		return http.StatusConflict, "state_conflict"
	case errors.Is(err, ErrLease):
		return http.StatusConflict, "lease_conflict"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func writeError(w http.ResponseWriter, err error) {
	status, code := statusFor(err)
	body := map[string]string{"error": err.Error(), "code": code}
	var depErr *DependencyError
	if errors.As(err, &depErr) {
		body["problems"] = ""
		writeJSONWithProblems(w, status, code, err.Error(), depErr.Problems)
		return
	}
	writeJSON(w, status, body)
}

func writeJSONWithProblems(w http.ResponseWriter, status int, code, msg string, problems []string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":    msg,
		"code":     code,
		"problems": problems,
	})
}
