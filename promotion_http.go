package releasetrain

import (
	"net/http"
	"strings"
)

// ---- 请求 DTO ----

type envPolicyDTO struct {
	Name      string         `json:"name"`
	Rules     []ApprovalRule `json:"rules"`
	Approvers []Approver     `json:"approvers"`
}

type promotionPolicyReq struct {
	Environments []envPolicyDTO `json:"environments"`
}

type createPromotionReq struct {
	TrainID   string `json:"train_id"`
	RequestID string `json:"request_id"`
}

type envApproveReq struct {
	Person    string `json:"person"`
	Role      string `json:"role"`
	RequestID string `json:"request_id"`
}

type claimReq struct {
	Worker    string `json:"worker"`
	RequestID string `json:"request_id"`
}

type deployReportReq struct {
	Token     string `json:"token"`
	Attempt   int    `json:"attempt"`
	Epoch     int    `json:"epoch"`
	Component string `json:"component"`
	Success   bool   `json:"success"`
	Version   string `json:"version"` // 成功必填且须等于快照版本；失败时为实际版本（可空）
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type rollbackReportReq struct {
	Token     string         `json:"token"`
	Attempt   int            `json:"attempt"`
	Epoch     int            `json:"epoch"`
	Component string         `json:"component"`
	Status    RollbackStatus `json:"status"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
}

type promotionActionReq struct {
	RequestID string `json:"request_id"`
}

// ---- 响应 DTO ----

type deployResultDTO struct {
	Component   string `json:"component"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	Success     bool   `json:"success"`
	Message     string `json:"message,omitempty"`
	Epoch       int    `json:"epoch"`
	At          string `json:"at,omitempty"`
}

type rollbackTaskDTO struct {
	Component string         `json:"component"`
	ToVersion string         `json:"to_version,omitempty"`
	Status    RollbackStatus `json:"status"`
	Message   string         `json:"message,omitempty"`
	At        string         `json:"at,omitempty"`
}

// envViewDTO 是单个环境的完整查询视图：执行前后版本、组件结果、回退进度、
// 审批依据与当前状态。
type envViewDTO struct {
	Name             string            `json:"name"`
	Order            int               `json:"order"`
	State            string            `json:"state"`
	Attempt          int               `json:"attempt"`
	BeforeVersions   map[string]string `json:"before_versions,omitempty"`
	TargetVersions   map[string]string `json:"target_versions"`
	CurrentVersions  map[string]string `json:"current_versions,omitempty"`
	ApprovalPolicy   PolicySnapshot    `json:"approval_policy"`
	Approvals        []EnvApproval     `json:"approvals,omitempty"`
	MissingApprovals []string          `json:"missing_approvals,omitempty"`
	Results          []deployResultDTO `json:"results,omitempty"`
	Rollback         []rollbackTaskDTO `json:"rollback,omitempty"`
	RollbackDone     int               `json:"rollback_done"`
	RollbackTotal    int               `json:"rollback_total"`
	EventID          string            `json:"event_id,omitempty"`
	FailReasons      []string          `json:"fail_reasons,omitempty"`
}

// promotionDTO 是晋级执行的对外视图，含当前阻断原因。
type promotionDTO struct {
	ID         string            `json:"id"`
	TrainID    string            `json:"train_id"`
	SnapshotID string            `json:"snapshot_id"`
	State      string            `json:"state"`
	Attempt    int               `json:"attempt"`
	Snapshot   map[string]string `json:"snapshot"`

	Environments   []envViewDTO `json:"environments"`
	BlockingReason string       `json:"blocking_reason,omitempty"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
}

func toPromotionDTO(p *Promotion) promotionDTO {
	installed := p.Installed()
	dto := promotionDTO{
		ID:             p.ID,
		TrainID:        p.TrainID,
		SnapshotID:     p.SnapshotID,
		State:          string(p.State()),
		Attempt:        p.Attempt,
		Snapshot:       versionsToStrings(p.Snapshot()),
		Environments:   []envViewDTO{},
		BlockingReason: p.BlockingReason(),
	}
	if !p.CreatedAt().IsZero() {
		dto.CreatedAt = p.CreatedAt().Format(timeRFC3339)
	}
	if !p.UpdatedAt().IsZero() {
		dto.UpdatedAt = p.UpdatedAt().Format(timeRFC3339)
	}
	for _, e := range p.Environments() {
		v := envViewDTO{
			Name:             e.Name,
			Order:            e.Order,
			State:            string(e.State),
			Attempt:          e.Attempt,
			BeforeVersions:   versionsToStrings(e.Baseline),
			TargetVersions:   versionsToStrings(p.Snapshot()),
			CurrentVersions:  versionsToStrings(installed[e.Name]),
			ApprovalPolicy:   e.Policy,
			Approvals:        append([]EnvApproval(nil), e.Approvals...),
			MissingApprovals: e.missingApprovals(),
			EventID:          e.EventID,
			FailReasons:      append([]string(nil), e.FailReasons...),
		}
		if e.Baseline == nil {
			v.BeforeVersions = nil
		}
		if installed[e.Name] == nil {
			v.CurrentVersions = nil
		}
		for _, r := range e.Results {
			rd := deployResultDTO{
				Component: r.Component, Success: r.Success,
				Message: r.Message, Epoch: r.Epoch,
			}
			if r.From != nil {
				rd.FromVersion = r.From.String()
			}
			if r.To != nil {
				rd.ToVersion = r.To.String()
			}
			if !r.At.IsZero() {
				rd.At = r.At.Format(timeRFC3339)
			}
			v.Results = append(v.Results, rd)
		}
		for _, t := range e.Rollback {
			td := rollbackTaskDTO{Component: t.Component, Status: t.Status, Message: t.Message}
			if t.To != nil {
				td.ToVersion = t.To.String()
			}
			if !t.At.IsZero() {
				td.At = t.At.Format(timeRFC3339)
			}
			v.Rollback = append(v.Rollback, td)
		}
		v.RollbackDone, v.RollbackTotal = rollbackProgress(e)
		dto.Environments = append(dto.Environments, v)
	}
	return dto
}

// ---- 处理器 ----

func (h *Handler) putPromotionPolicy(w http.ResponseWriter, r *http.Request) {
	var req promotionPolicyReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	pol := &PromotionPolicy{Environments: make([]EnvPolicy, 0, len(req.Environments))}
	for _, e := range req.Environments {
		pol.Environments = append(pol.Environments, EnvPolicy{
			Name: strings.TrimSpace(e.Name), Rules: e.Rules, Approvers: e.Approvers,
		})
	}
	if err := h.svc.PutPromotionPolicy(pol); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getPromotionPolicy(w http.ResponseWriter, _ *http.Request) {
	p := h.svc.GetPromotionPolicy()
	if p == nil {
		writeJSON(w, http.StatusOK, PromotionPolicy{Environments: []EnvPolicy{}})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) createPromotion(w http.ResponseWriter, r *http.Request) {
	var req createPromotionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := h.svc.CreatePromotion(strings.TrimSpace(req.TrainID), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPromotionDTO(p))
}

func (h *Handler) listPromotions(w http.ResponseWriter, _ *http.Request) {
	ps, err := h.svc.ListPromotions()
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]promotionDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, toPromotionDTO(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getPromotion(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.GetPromotion(r.PathValue("pid"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPromotionDTO(p))
}

func (h *Handler) approveEnv(w http.ResponseWriter, r *http.Request) {
	var req envApproveReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := h.svc.ApprovePromotionEnv(r.PathValue("pid"), r.PathValue("env"),
		strings.TrimSpace(req.Person), strings.TrimSpace(req.Role), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPromotionDTO(p))
}

func (h *Handler) claimDeploy(w http.ResponseWriter, r *http.Request) {
	var req claimReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	l, err := h.svc.ClaimDeployment(r.PathValue("pid"), r.PathValue("env"),
		strings.TrimSpace(req.Worker), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (h *Handler) reportDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployReportReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	in := DeployReceiptInput{
		PromotionID: r.PathValue("pid"),
		Environment: r.PathValue("env"),
		Token:       strings.TrimSpace(req.Token),
		Attempt:     req.Attempt,
		Epoch:       req.Epoch,
		Component:   strings.TrimSpace(req.Component),
		Success:     req.Success,
		Message:     req.Message,
		RequestID:   strings.TrimSpace(req.RequestID),
	}
	if v := strings.TrimSpace(req.Version); v != "" {
		ver, err := ParseVersion(v)
		if err != nil {
			writeError(w, err)
			return
		}
		in.Version = &ver
	}
	res, err := h.svc.ReportDeployment(in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) claimRollback(w http.ResponseWriter, r *http.Request) {
	var req claimReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	l, err := h.svc.ClaimRollback(r.PathValue("pid"), r.PathValue("env"), strings.TrimSpace(req.Worker))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (h *Handler) reportRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackReportReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := h.svc.ReportRollback(RollbackReceiptInput{
		PromotionID: r.PathValue("pid"),
		Environment: r.PathValue("env"),
		Token:       strings.TrimSpace(req.Token),
		Attempt:     req.Attempt,
		Epoch:       req.Epoch,
		Component:   strings.TrimSpace(req.Component),
		Status:      req.Status,
		Message:     req.Message,
		RequestID:   strings.TrimSpace(req.RequestID),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) cancelPromotion(w http.ResponseWriter, r *http.Request) {
	var req promotionActionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := h.svc.CancelPromotion(r.PathValue("pid"), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPromotionDTO(p))
}

func (h *Handler) retryPromotion(w http.ResponseWriter, r *http.Request) {
	var req promotionActionReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := h.svc.RetryPromotion(r.PathValue("pid"), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPromotionDTO(p))
}
