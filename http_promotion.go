package releasetrain

import (
	"fmt"
	"net/http"
	"strings"
)

// ---- 环境晋级 HTTP ----

type envPolicyReq struct {
	Environments []struct {
		Name      string         `json:"name"`
		Rules     []ApprovalRule `json:"rules"`
		Approvers []Approver     `json:"approvers"`
	} `json:"environments"`
}

type promotionActionReq struct {
	Person    string `json:"person,omitempty"`
	Role      string `json:"role,omitempty"`
	RequestID string `json:"request_id"`
}

type claimReq struct {
	Worker string `json:"worker"`
}

type receiptReq struct {
	AttemptNo   int    `json:"attempt_no"`
	LeaseNo     int    `json:"lease_no"`
	Environment string `json:"environment,omitempty"` // 可由 path 提供
	RequestID   string `json:"request_id"`
	Receipts    []struct {
		Component       string `json:"component"`
		Success         bool   `json:"success"`
		DeployedVersion string `json:"deployed_version,omitempty"`
		Message         string `json:"message,omitempty"`
	} `json:"receipts"`
}

type rollbackReportReq struct {
	AttemptNo   int    `json:"attempt_no"`
	LeaseNo     int    `json:"lease_no"`
	Environment string `json:"environment"`
	Component   string `json:"component"`
	Success     bool   `json:"success"`
	Message     string `json:"message,omitempty"`
}

func (h *Handler) putEnvironmentPolicy(w http.ResponseWriter, r *http.Request) {
	var req envPolicyReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p := &EnvironmentPolicy{}
	for _, e := range req.Environments {
		p.Environments = append(p.Environments, EnvPolicy{
			Name:      e.Name,
			Rules:     e.Rules,
			Approvers: e.Approvers,
		})
	}
	if err := h.svc.PutEnvironmentPolicy(p); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getEnvironmentPolicy(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.GetEnvironmentPolicy())
}

func (h *Handler) createPromotion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := h.svc.CreatePromotion(r.PathValue("id"), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := h.svc.GetPromotionView(p.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *Handler) listPromotions(w http.ResponseWriter, _ *http.Request) {
	ps, err := h.svc.ListPromotions()
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]*PromotionView, 0, len(ps))
	for _, p := range ps {
		view, err := h.svc.GetPromotionView(p.ID)
		if err != nil {
			writeError(w, err)
			return
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getPromotion(w http.ResponseWriter, r *http.Request) {
	// 详细查询视图（含前后版本、组件结果、回退、审批依据、阻断原因）。
	view, err := h.svc.GetPromotionView(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) promotionApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Environment string `json:"environment"`
		Person      string `json:"person"`
		Role        string `json:"role"`
		RequestID   string `json:"request_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, err := h.svc.ApprovePromotion(PromotionApproveInput{
		PromotionID: r.PathValue("id"),
		Environment: strings.TrimSpace(req.Environment),
		Person:      strings.TrimSpace(req.Person),
		Role:        strings.TrimSpace(req.Role),
		RequestID:   strings.TrimSpace(req.RequestID),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := h.svc.GetPromotionView(p.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) cancelPromotion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p, started, err := h.svc.CancelPromotion(r.PathValue("id"), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := h.svc.GetPromotionView(p.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"promotion": view, "rollback_started": started})
}

func (h *Handler) newAttempt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	res, _, err := h.svc.StartNewAttempt(r.PathValue("id"), strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := h.svc.GetPromotionView(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"result": res, "promotion": view})
}

func (h *Handler) claimDeployment(w http.ResponseWriter, r *http.Request) {
	var req claimReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	lease, err := h.svc.ClaimDeployment(r.PathValue("id"), r.PathValue("env"), strings.TrimSpace(req.Worker))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (h *Handler) submitReceipts(w http.ResponseWriter, r *http.Request) {
	var req receiptReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.AttemptNo <= 0 || req.LeaseNo <= 0 {
		writeError(w, invalidArg("attempt_no and lease_no are required"))
		return
	}
	entries := make([]ReceiptEntry, 0, len(req.Receipts))
	for _, rc := range req.Receipts {
		entry := ReceiptEntry{Component: strings.TrimSpace(rc.Component), Success: rc.Success, Message: rc.Message}
		if strings.TrimSpace(rc.Component) == "" {
			writeError(w, invalidArg("receipt component is required"))
			return
		}
		if strings.TrimSpace(rc.DeployedVersion) != "" {
			ver, err := ParseVersion(rc.DeployedVersion)
			if err != nil {
				writeError(w, err)
				return
			}
			entry.DeployedVersion = &ver
		}
		entries = append(entries, entry)
	}
	env := r.PathValue("env")
	if strings.TrimSpace(env) == "" {
		env = strings.TrimSpace(req.Environment)
	}
	out, err := h.svc.SubmitReceipts(r.PathValue("id"), req.AttemptNo, req.LeaseNo,
		env, entries, strings.TrimSpace(req.RequestID))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) claimRollback(w http.ResponseWriter, r *http.Request) {
	var req claimReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	lease, err := h.svc.ClaimRollback(r.PathValue("id"), strings.TrimSpace(req.Worker))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (h *Handler) reportRollback(w http.ResponseWriter, r *http.Request) {
	var req rollbackReportReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	out, err := h.svc.ReportRollback(r.PathValue("id"), req.AttemptNo, req.LeaseNo,
		strings.TrimSpace(req.Environment), strings.TrimSpace(req.Component), req.Success, req.Message)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func invalidArg(msg string) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, msg)
}
