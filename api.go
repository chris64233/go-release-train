package releasetrain

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Server 暴露发布列车的 HTTP 接口。
type Server struct {
	svc *Service
	mux *http.ServeMux
}

// NewServer 创建 HTTP 服务。
func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 返回可挂载的 http.Handler。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	m.HandleFunc("POST /v1/components", s.idempotent(s.registerComponent))
	m.HandleFunc("GET /v1/components", s.listComponents)
	m.HandleFunc("GET /v1/components/{name}", s.getComponent)
	m.HandleFunc("POST /v1/components/{name}/versions", s.idempotent(s.registerVersion))
	m.HandleFunc("GET /v1/components/{name}/versions", s.listVersions)

	m.HandleFunc("PUT /v1/policy", s.setPolicy)
	m.HandleFunc("GET /v1/policy", s.getPolicy)

	m.HandleFunc("POST /v1/trains", s.idempotent(s.createTrain))
	m.HandleFunc("GET /v1/trains", s.listTrains)
	m.HandleFunc("GET /v1/trains/{id}", s.getTrain)
	m.HandleFunc("PUT /v1/trains/{id}/candidates", s.idempotent(s.setCandidates))
	m.HandleFunc("POST /v1/trains/{id}/freeze", s.idempotent(s.freezeTrain))
	m.HandleFunc("POST /v1/trains/{id}/approvals", s.idempotent(s.approve))
	m.HandleFunc("GET /v1/trains/{id}/approvals", s.approvalSummary)
	m.HandleFunc("POST /v1/trains/{id}/cancel", s.idempotent(s.cancelTrain))
	m.HandleFunc("POST /v1/trains/{id}/release", s.idempotent(s.releaseTrain))

	m.HandleFunc("GET /v1/outbox", s.listOutbox)
	m.HandleFunc("POST /v1/outbox/{id}/publish", s.idempotent(s.publishOutbox))
}

// ---------------------------------------------------------------------------
// 请求/响应结构
// ---------------------------------------------------------------------------

type registerComponentReq struct {
	Name string `json:"name"`
}

type dependencyDTO struct {
	Component  string `json:"component"`
	Constraint string `json:"constraint"`
}

type registerVersionReq struct {
	Version      string          `json:"version"`
	Dependencies []dependencyDTO `json:"dependencies"`
}

type candidateDTO struct {
	Component string `json:"component"`
	Version   string `json:"version"`
}

type createTrainReq struct {
	ID         string         `json:"id"`
	Candidates []candidateDTO `json:"candidates"`
}

type setCandidatesReq struct {
	Candidates       []candidateDTO `json:"candidates"`
	ExpectedRevision int64          `json:"expected_revision"`
}

type approveReq struct {
	Person           string `json:"person"`
	Role             string `json:"role"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type cancelReq struct {
	Reason           string `json:"reason"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type policyReq struct {
	Rules []policyRuleDTO `json:"rules"`
}

type policyRuleDTO struct {
	Role      string   `json:"role"`
	Members   []string `json:"members"`
	Threshold int      `json:"threshold"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Current int64  `json:"current_revision,omitempty"`
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

func (s *Server) registerComponent(w http.ResponseWriter, r *http.Request) {
	var req registerComponentReq
	if !decode(w, r, &req) {
		return
	}
	c, err := s.svc.RegisterComponent(firstNonEmpty(req.Name, r.PathValue("name")))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) listComponents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"components": s.svc.ListComponents()})
}

func (s *Server) getComponent(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.GetComponent(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) registerVersion(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req registerVersionReq
	if !decode(w, r, &req) {
		return
	}
	deps := make([]Dependency, 0, len(req.Dependencies))
	for _, d := range req.Dependencies {
		deps = append(deps, Dependency{Component: d.Component, Constraint: d.Constraint})
	}
	v, err := s.svc.RegisterVersion(name, req.Version, deps)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.svc.ListVersions(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request) {
	var req policyReq
	if !decode(w, r, &req) {
		return
	}
	rules := make([]ApprovalRule, 0, len(req.Rules))
	for _, ru := range req.Rules {
		rules = append(rules, ApprovalRule{Role: ru.Role, Members: ru.Members, Threshold: ru.Threshold})
	}
	out, err := s.svc.SetPolicy(rules)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": out})
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"policy": s.svc.GetPolicy()})
}

func (s *Server) createTrain(w http.ResponseWriter, r *http.Request) {
	var req createTrainReq
	if !decode(w, r, &req) {
		return
	}
	t, err := s.svc.CreateTrain(req.ID, toCandidates(req.Candidates))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTrains(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"trains": s.svc.ListTrains()})
}

func (s *Server) getTrain(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.GetTrain(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) setCandidates(w http.ResponseWriter, r *http.Request) {
	var req setCandidatesReq
	if !decode(w, r, &req) {
		return
	}
	rev, ok := expectedRevision(w, r, req.ExpectedRevision)
	if !ok {
		return
	}
	t, err := s.svc.SetCandidates(r.PathValue("id"), toCandidates(req.Candidates), rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) freezeTrain(w http.ResponseWriter, r *http.Request) {
	rev, ok := expectedRevision(w, r, 0)
	if !ok {
		return
	}
	t, err := s.svc.FreezeTrain(r.PathValue("id"), rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !decode(w, r, &req) {
		return
	}
	rev, ok := expectedRevision(w, r, req.ExpectedRevision)
	if !ok {
		return
	}
	t, a, err := s.svc.Approve(r.PathValue("id"), req.Person, req.Role, rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"train": t, "approval": a})
}

func (s *Server) approvalSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.ApprovalSummary(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) cancelTrain(w http.ResponseWriter, r *http.Request) {
	var req cancelReq
	if !decode(w, r, &req) {
		return
	}
	rev, ok := expectedRevision(w, r, req.ExpectedRevision)
	if !ok {
		return
	}
	t, err := s.svc.CancelTrain(r.PathValue("id"), req.Reason, rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) releaseTrain(w http.ResponseWriter, r *http.Request) {
	rev, ok := expectedRevision(w, r, 0)
	if !ok {
		return
	}
	res, err := s.svc.ReleaseTrain(r.PathValue("id"), rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listOutbox(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"events": s.svc.ListOutbox()})
}

func (s *Server) publishOutbox(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, fail(KindValidation, "publishOutbox", "invalid outbox id"))
		return
	}
	if err := s.svc.MarkOutboxPublished(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"published": true, "id": id})
}

// ---------------------------------------------------------------------------
// 幂等中间件
// ---------------------------------------------------------------------------

// idempotent 用 Idempotency-Key 头包装写操作：同一键 + 同一请求体重放原响应；
// 同一键但不同请求体返回 409 幂等冲突。
func (s *Server) idempotent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			h(w, r)
			return
		}
		body := readBody(r)
		fp := Fingerprint(r.Method, r.URL.Path, body)
		if rec, found := s.svc.LoadIdempotency(key); found {
			if rec.Fingerprint != fp {
				writeError(w, fail(KindIdempotency, r.URL.Path,
					"Idempotency-Key %q was already used with a different request", key))
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			writeJSON(w, rec.Status, rec.Response)
			return
		}
		rec := newRecordingWriter(w)
		h(rec, r.WithContext(r.Context()))
		if rec.status >= 200 && rec.status < 300 {
			s.svc.SaveIdempotency(key, fp, rec.status, rec.buf.Bytes())
		}
	}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func toCandidates(in []candidateDTO) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		out = append(out, Candidate{Component: c.Component, Version: c.Version})
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// expectedRevision 解析版本条件：查询参数 expected_revision、X-Expected-Revision
// 或 If-Match 头，或请求体字段（调用方传入）。三者都没有时返回 0，表示不强制。
func expectedRevision(w http.ResponseWriter, r *http.Request, fromBody int64) (int64, bool) {
	q := strings.TrimSpace(r.URL.Query().Get("expected_revision"))
	h := strings.TrimSpace(r.Header.Get("X-Expected-Revision"))
	if h == "" {
		h = strings.TrimSpace(r.Header.Get("If-Match"))
	}
	switch {
	case q != "":
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 1 {
			writeError(w, fail(KindValidation, "expectedRevision", "invalid expected_revision %q", q))
			return 0, false
		}
		return n, true
	case h != "":
		n, err := strconv.ParseInt(h, 10, 64)
		if err != nil || n < 1 {
			writeError(w, fail(KindValidation, "expectedRevision", "invalid revision header %q", h))
			return 0, false
		}
		return n, true
	case fromBody > 0:
		return fromBody, true
	default:
		return 0, true
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := readBody(r)
	if len(body) == 0 {
		return true // 允许空体，走零值
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeError(w, fail(KindValidation, r.URL.Path, "invalid JSON body: %v", err))
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Kind: KindValidation, Msg: err.Error(), err: ErrValidation}
	}
	status := http.StatusInternalServerError
	switch e.Kind {
	case KindValidation:
		status = http.StatusBadRequest
	case KindNotFound:
		status = http.StatusNotFound
	case KindDependency:
		status = http.StatusUnprocessableEntity
	case KindApproval:
		status = http.StatusForbidden
	case KindState, KindIdempotency:
		status = http.StatusConflict
	case KindVersion:
		status = http.StatusPreconditionFailed
	}
	writeJSON(w, status, errorBody{Error: errorDetail{
		Kind:    e.Kind,
		Message: e.Msg,
		Current: e.Current,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
