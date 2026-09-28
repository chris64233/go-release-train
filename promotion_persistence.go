package releasetrain

import "time"

// promotionWire 是 Promotion 的 JSON 线格式（state/snapshot/envs/installed 为私有字段）。
type promotionWire struct {
	ID         string                        `json:"id"`
	TrainID    string                        `json:"train_id"`
	SnapshotID string                        `json:"snapshot_id"`
	State      PromotionState                `json:"state"`
	Attempt    int                           `json:"attempt"`
	Snapshot   map[string]Version            `json:"snapshot"`
	Envs       []*EnvExecution               `json:"environments"`
	Installed  map[string]map[string]Version `json:"installed,omitempty"`
	CreatedAt  time.Time                     `json:"created_at"`
	UpdatedAt  time.Time                     `json:"updated_at"`
}

func toPromotionWire(p *Promotion) *promotionWire {
	return &promotionWire{
		ID:         p.ID,
		TrainID:    p.TrainID,
		SnapshotID: p.SnapshotID,
		State:      p.state,
		Attempt:    p.Attempt,
		Snapshot:   p.snapshot,
		Envs:       p.envs,
		Installed:  p.installed,
		CreatedAt:  p.createdAt,
		UpdatedAt:  p.updatedAt,
	}
}

func fromPromotionWire(w *promotionWire) *Promotion {
	return &Promotion{
		ID:         w.ID,
		TrainID:    w.TrainID,
		SnapshotID: w.SnapshotID,
		state:      w.State,
		Attempt:    w.Attempt,
		snapshot:   w.Snapshot,
		envs:       w.Envs,
		installed:  w.Installed,
		createdAt:  w.CreatedAt,
		updatedAt:  w.UpdatedAt,
	}
}

// clonePromotion 深拷贝晋级聚合（事务副本与查询返回值都经它隔离）。
func clonePromotion(p *Promotion) *Promotion {
	cp := *p
	cp.snapshot = cloneCandidates(p.snapshot)
	cp.envs = make([]*EnvExecution, len(p.envs))
	for i, e := range p.envs {
		cp.envs[i] = cloneEnvExecution(e)
	}
	cp.installed = p.Installed()
	return &cp
}
