package releasetrain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---- 晋级策略 ----

// PutPromotionPolicy 设置当前跨环境晋级策略（环境严格顺序 + 各环境审批门槛）。
// 只影响之后创建的晋级执行；已创建的晋级使用其冻结副本。
func (s *Service) PutPromotionPolicy(pol *PromotionPolicy) error {
	if err := validatePromotionPolicy(pol); err != nil {
		return err
	}
	cp := clonePromotionPolicy(pol)
	return s.store.Update(func(st *storedState) error {
		st.promotionPolicy = cp
		return nil
	})
}

// GetPromotionPolicy 返回当前晋级策略（未设置时为 nil）。
func (s *Service) GetPromotionPolicy() *PromotionPolicy {
	var out *PromotionPolicy
	_ = s.store.View(func(st *storedState) error {
		if st.promotionPolicy != nil {
			out = clonePromotionPolicy(st.promotionPolicy)
		}
		return nil
	})
	return out
}

// ---- 创建晋级执行 ----

// CreatePromotion 在已冻结并放行的发布列车上创建一次跨环境逐级晋级执行。
// 创建瞬间冻结三样东西：列车快照副本、环境顺序、各环境审批策略。
//   - 列车必须已放行（未放行不能开始）；
//   - 冻结快照必须完整（非空，且每个快照版本仍可在登记注册表中找到）。
func (s *Service) CreatePromotion(trainID, requestID string) (*Promotion, error) {
	fp := fingerprint(struct {
		TrainID string `json:"train_id"`
	}{trainID})

	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "create_promotion", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}
		if t.State() != StateReleased {
			return fmt.Errorf("%w: promotion can only start from a released train; train %s is %s",
				ErrStateConflict, trainID, t.State())
		}
		snapshot := t.Snapshot()
		if len(snapshot) == 0 || t.SnapshotID() == "" {
			return fmt.Errorf("%w: train %s snapshot is incomplete (empty snapshot)", ErrDependency, trainID)
		}
		// 依赖快照完整性：快照引用的每个版本必须仍在登记注册表中。
		reg := registryAdapter{st.versions}
		var missing []string
		for comp, ver := range snapshot {
			if _, ok := reg.constraintsOf(comp, ver); !ok {
				missing = append(missing, fmt.Sprintf("component %s version %s", comp, ver))
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("%w: train %s snapshot is no longer complete, unregistered: %v",
				ErrDependency, trainID, missing)
		}
		if st.promotionPolicy == nil {
			return fmt.Errorf("%w: no promotion policy configured; creating promotion for train %s requires one",
				ErrInvalidArgument, trainID)
		}
		p := newPromotion("promo_"+newID(), t.ID, t.SnapshotID(), snapshot, *clonePromotionPolicy(st.promotionPolicy), s.now().UTC())
		st.promotions[p.ID] = p
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "create_promotion", p.ID, fp, toPromotionWire(p))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func decodePromotion(raw json.RawMessage) *Promotion {
	var w promotionWire
	if err := json.Unmarshal(raw, &w); err != nil {
		panic(err)
	}
	return fromPromotionWire(&w)
}

func requirePromotion(st *storedState, id string) (*Promotion, error) {
	p, ok := st.promotions[id]
	if !ok {
		return nil, fmt.Errorf("%w: promotion %s", ErrNotFound, id)
	}
	return p, nil
}

// ---- 环境审批 ----

// ApprovePromotionEnv 为指定环境（必须是当前开放环境）提交审批。
// 资格只认创建晋级时冻结的该环境策略快照；同一 (person, role) 在同一尝试幂等。
func (s *Service) ApprovePromotionEnv(promotionID, env, person, role, requestID string) (*Promotion, error) {
	if strings.TrimSpace(person) == "" || strings.TrimSpace(role) == "" {
		return nil, fmt.Errorf("%w: person and role are required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		PromotionID string `json:"promotion_id"`
		Env         string `json:"environment"`
		Person      string `json:"person"`
		Role        string `json:"role"`
	}{promotionID, env, person, role})

	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "approve_env", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		if _, err := p.ApproveEnv(env, person, role, s.now().UTC()); err != nil {
			return err
		}
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "approve_env", p.ID, fp, toPromotionWire(p))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 部署领取 / 回执 ----

// ClaimDeployment 工作者领取某环境的整列车部署（已开放且审批门槛满足）。
// 重复领取即接管：返回新租约 token 与递增 epoch，旧租约即刻失效。
func (s *Service) ClaimDeployment(promotionID, env, worker, requestID string) (*DeployLease, error) {
	if strings.TrimSpace(worker) == "" {
		return nil, fmt.Errorf("%w: worker id is required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		PromotionID string `json:"promotion_id"`
		Env         string `json:"environment"`
		Worker      string `json:"worker"`
	}{promotionID, env, worker})

	var out *DeployLease
	err := s.store.Update(func(st *storedState) error {
		// 接管会换发租约，因此 request 重放只允许返回首次发放的同一租约。
		if replayed, raw, err := checkIdem(st.idem, requestID, "claim_deploy", fp); err != nil {
			return err
		} else if replayed {
			var l DeployLease
			if err := json.Unmarshal(raw, &l); err != nil {
				return err
			}
			out = &l
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		l, err := p.ClaimDeploy(env, worker, s.now().UTC())
		if err != nil {
			return err
		}
		out = l
		return saveIdem(st.idem, requestID, "claim_deploy", p.ID, fp, l)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeployReceiptInput 是单个组件的部署回执。
type DeployReceiptInput struct {
	PromotionID string
	Environment string
	Worker      string
	Token       string
	Attempt     int
	Epoch       int
	Component   string
	Success     bool
	Version     *Version // 成功时必须等于快照版本；失败时可选填实际版本
	Message     string
	RequestID   string
}

// ReportDeployment 提交部署回执。只有当前尝试的当前租约可以提交；
// 重复回执与请求重试返回已有结果；失败立即触发整环境回退。
func (s *Service) ReportDeployment(in DeployReceiptInput) (*DeployReceiptResult, error) {
	if strings.TrimSpace(in.Token) == "" || in.Attempt <= 0 || in.Epoch <= 0 {
		return nil, fmt.Errorf("%w: lease token, attempt and epoch are required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		PromotionID string `json:"promotion_id"`
		Env         string `json:"environment"`
		Component   string `json:"component"`
		Success     bool   `json:"success"`
	}{in.PromotionID, in.Environment, in.Component, in.Success})

	var out *DeployReceiptResult
	err := s.store.Update(func(st *storedState) error {
		// 同 request 重试：重放上次处理结果（标记为幂等重放）。
		if replayed, raw, err := checkIdem(st.idem, in.RequestID, "report_deploy", fp); err != nil {
			return err
		} else if replayed {
			var r DeployReceiptResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			r.Idempotent = true
			out = &r
			return nil
		}
		p, err := requirePromotion(st, in.PromotionID)
		if err != nil {
			return err
		}
		res, err := p.ReportDeploy(in.Environment, in.Token, in.Attempt, in.Epoch,
			in.Component, in.Success, in.Version, in.Message, s.now().UTC())
		if err != nil {
			return err
		}
		// 环境晋级与 outbox 事件在同一事务内落定：聚合只持有稳定 EventID，
		// 此处据此补齐唯一事件（重复回执/重入不会产生第二条）。
		if res.EnvPromoted {
			if e, ferr := p.env(in.Environment); ferr == nil {
				st.outbox = appendOutboxEvent(st.outbox, p, e)
			}
		}
		out = res
		return saveIdem(st.idem, in.RequestID, "report_deploy", p.ID, fp, res)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// appendOutboxEvent 根据环境执行重建该环境唯一的晋级事件并写入 outbox。
func appendOutboxEvent(events []OutboxEvent, p *Promotion, e *EnvExecution) []OutboxEvent {
	for _, ev := range events {
		if ev.ID == e.EventID {
			return events // 稳定一条，绝不重复
		}
	}
	from := map[string]string{}
	for c, v := range e.Baseline {
		from[c] = v.String()
	}
	to := map[string]string{}
	for c, v := range p.Snapshot() {
		to[c] = v.String()
	}
	payload, _ := json.Marshal(map[string]any{
		"promotion_id": p.ID,
		"train_id":     p.TrainID,
		"snapshot_id":  p.SnapshotID,
		"environment":  e.Name,
		"attempt":      e.Attempt,
		"from":         from,
		"to":           to,
	})
	return append(events, OutboxEvent{
		ID:        e.EventID,
		TrainID:   p.TrainID,
		Type:      "environment.promoted",
		Payload:   json.RawMessage(payload),
		CreatedAt: e.PromotedAt,
	})
}

// ---- 回退领取 / 回执 ----

// ClaimRollback 领取（或接管）某环境的回退工作，返回尚未恢复的组件任务。
func (s *Service) ClaimRollback(promotionID, env, worker string) (*RollbackLease, error) {
	if strings.TrimSpace(worker) == "" {
		return nil, fmt.Errorf("%w: worker id is required", ErrInvalidArgument)
	}
	var out *RollbackLease
	err := s.store.Update(func(st *storedState) error {
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		l, err := p.ClaimRollback(env, worker, s.now().UTC())
		if err != nil {
			return err
		}
		out = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RollbackReceiptInput 是单个组件的回退回执。
type RollbackReceiptInput struct {
	PromotionID string
	Environment string
	Token       string
	Attempt     int
	Epoch       int
	Component   string
	Status      RollbackStatus
	Message     string
	RequestID   string
}

// ReportRollback 提交回退结果。全部组件恢复成功后，部署失败路径落 failed
// （可创建新尝试），取消打断路径落 cancelled（终态）。
func (s *Service) ReportRollback(in RollbackReceiptInput) (*RollbackReceiptResult, error) {
	if strings.TrimSpace(in.Token) == "" || in.Attempt <= 0 || in.Epoch <= 0 {
		return nil, fmt.Errorf("%w: lease token, attempt and epoch are required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		PromotionID string         `json:"promotion_id"`
		Env         string         `json:"environment"`
		Component   string         `json:"component"`
		Status      RollbackStatus `json:"status"`
	}{in.PromotionID, in.Environment, in.Component, in.Status})

	var out *RollbackReceiptResult
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, in.RequestID, "report_rollback", fp); err != nil {
			return err
		} else if replayed {
			var r RollbackReceiptResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			out = &r
			return nil
		}
		p, err := requirePromotion(st, in.PromotionID)
		if err != nil {
			return err
		}
		res, err := p.ReportRollback(in.Environment, in.Token, in.Attempt, in.Epoch,
			in.Component, in.Status, in.Message, s.now().UTC())
		if err != nil {
			return err
		}
		out = res
		return saveIdem(st.idem, in.RequestID, "report_rollback", p.ID, fp, res)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 取消 / 重试 ----

// CancelPromotion 取消晋级执行（可与审批、部署回执并发，由事务串行化）。
// 部署进行中时先进入 cancelling 回退，回退完成才 cancelled；重复取消幂等。
func (s *Service) CancelPromotion(promotionID, requestID string) (*Promotion, error) {
	fp := fingerprint(struct {
		PromotionID string `json:"promotion_id"`
	}{promotionID})

	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "cancel_promotion", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		if _, err := p.Cancel(s.now().UTC()); err != nil {
			return err
		}
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "cancel_promotion", p.ID, fp, toPromotionWire(p))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RetryPromotion 在失败后开启新尝试：必须沿用原列车快照，尝试号 +1，
// 审批与部署进度全部重来，已晋级环境保持不动。
func (s *Service) RetryPromotion(promotionID, requestID string) (*Promotion, error) {
	fp := fingerprint(struct {
		PromotionID string `json:"promotion_id"`
	}{promotionID})

	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "retry_promotion", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		if err := p.Retry(s.now().UTC()); err != nil {
			return err
		}
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "retry_promotion", p.ID, fp, toPromotionWire(p))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 查询 ----

// GetPromotion 返回晋级执行详情（深拷贝）。
func (s *Service) GetPromotion(id string) (*Promotion, error) {
	var out *Promotion
	err := s.store.View(func(st *storedState) error {
		p, err := requirePromotion(st, id)
		if err != nil {
			return err
		}
		out = clonePromotion(p)
		return nil
	})
	return out, err
}

// ListPromotions 返回全部晋级执行（按 ID 排序）。
func (s *Service) ListPromotions() ([]*Promotion, error) {
	var out []*Promotion
	err := s.store.View(func(st *storedState) error {
		ids := make([]string, 0, len(st.promotions))
		for id := range st.promotions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out = make([]*Promotion, 0, len(ids))
		for _, id := range ids {
			out = append(out, clonePromotion(st.promotions[id]))
		}
		return nil
	})
	return out, err
}
