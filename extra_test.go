package releasetrain

import (
	"errors"
	"testing"
)

func TestOutboxDispatchLifecycle(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)
	res, err := svc.Release(tr.ID, 0, "")
	if err != nil {
		t.Fatal(err)
	}

	events, _ := svc.PendingOutbox()
	if len(events) != 1 {
		t.Fatalf("pending = %d", len(events))
	}
	if err := svc.MarkOutboxDispatched(res.EventID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := svc.PendingOutbox(); len(pending) != 0 {
		t.Fatalf("pending after dispatch = %d", len(pending))
	}
	if err := svc.MarkOutboxDispatched("evt_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dispatch missing event: %v", err)
	}
}

func TestIdempotencyReplayAcrossActions(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)

	// 冻结 request id 重放返回同一冻结结果。
	f1, err := svc.Freeze(tr.ID, 0, "freeze-1")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := svc.Freeze(tr.ID, 0, "freeze-1")
	if err != nil {
		t.Fatal(err)
	}
	if f2.Version != f1.Version || f2.SnapshotID() != f1.SnapshotID() {
		t.Fatal("freeze replay mismatch")
	}

	// 审批 request id 重放。
	a1, err := svc.Approve(tr.ID, "alice", "qa", 0, "appr-1")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := svc.Approve(tr.ID, "alice", "qa", 0, "appr-1")
	if err != nil {
		t.Fatal(err)
	}
	if a2.Version != a1.Version {
		t.Fatal("approve replay bumped version")
	}

	// cancel request id 重放返回已取消状态。
	c1, err := svc.Cancel(tr.ID, 0, "cancel-1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := svc.Cancel(tr.ID, 0, "cancel-1")
	if err != nil {
		t.Fatal(err)
	}
	if c2.State() != StateCancelled || c2.Version != c1.Version {
		t.Fatal("cancel replay mismatch")
	}
}

func TestReleaseRequestIDReplayAfterRelease(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)
	freezeAndApprove(t, svc, tr.ID)

	first, err := svc.Release(tr.ID, 0, "rel-key")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := svc.Release(tr.ID, 0, "rel-key")
	if err != nil {
		t.Fatal(err)
	}
	if replay.EventID != first.EventID {
		t.Fatal("release key replay mismatch")
	}
}

func TestSetCandidateRequestIDReplay(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr, _ := svc.CreateTrain("t", "")

	in := SetCandidateInput{TrainID: tr.ID, Component: "order", Version: v("2.1.0"), RequestID: "set-1"}
	s1, err := svc.SetCandidate(in)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := svc.SetCandidate(in)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version != s1.Version {
		t.Fatal("set candidate replay bumped version")
	}
}

func TestRemoveCandidateFlow(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	defaultPolicy(t, svc)
	tr := buildCompatibleTrain(t, svc)

	got, err := svc.RemoveCandidate(tr.ID, "gateway", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Candidates()["gateway"]; ok {
		t.Fatal("gateway still present")
	}
	if _, err := svc.RemoveCandidate(tr.ID, "ghost", 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing: %v", err)
	}
	// 冻结后移除被拒。
	if _, err := svc.Freeze(tr.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RemoveCandidate(tr.ID, "order", 0, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("remove after freeze: %v", err)
	}
}

func TestPolicyValidation(t *testing.T) {
	svc := newTestService(t)
	cases := []struct {
		name      string
		rules     []ApprovalRule
		approvers []Approver
	}{
		{"empty role", []ApprovalRule{{Role: "", Need: 1}}, nil},
		{"need zero", []ApprovalRule{{Role: "qa", Need: 0}}, nil},
		{"duplicate rule", []ApprovalRule{{Role: "qa", Need: 1}, {Role: "qa", Need: 2}}, nil},
		{"approver without rule", nil, []Approver{{Person: "a", Role: "qa"}}},
		{"not enough approvers", []ApprovalRule{{Role: "qa", Need: 2}},
			[]Approver{{Person: "a", Role: "qa"}}},
		{"duplicate approver seat", []ApprovalRule{{Role: "qa", Need: 1}},
			[]Approver{{Person: "a", Role: "qa"}, {Person: "a", Role: "qa"}}},
	}
	for _, c := range cases {
		if err := svc.PutPolicy(c.rules, c.approvers); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: want invalid argument, got %v", c.name, err)
		}
	}
}

func TestFreezeWithoutPolicy(t *testing.T) {
	svc := newTestService(t)
	registerStack(t, svc)
	tr := buildCompatibleTrain(t, svc)
	_, err := svc.Freeze(tr.ID, 0, "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("freeze without policy: %v", err)
	}
}

func TestFreezeEmptyTrain(t *testing.T) {
	svc := newTestService(t)
	defaultPolicy(t, svc)
	tr, _ := svc.CreateTrain("empty", "")
	_, err := svc.Freeze(tr.ID, 0, "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("freeze empty train: %v", err)
	}
}

func TestRegisterVersionValidation(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.RegisterVersion(RegisterVersionInput{Component: "", Version: v("1.0.0")}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty component: %v", err)
	}
	if _, err := svc.RegisterVersion(RegisterVersionInput{
		Component: "x", Version: v("1.0.0"),
		Constraints: []Constraint{{Component: "", Op: OpEq, Version: v("1.0.0")}},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty constraint component: %v", err)
	}
	if _, err := svc.RegisterVersion(RegisterVersionInput{
		Component: "x", Version: v("1.0.0"),
		Constraints: []Constraint{{Component: "y", Op: "~", Version: v("1.0.0")}},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad op: %v", err)
	}
}

func TestCreateTrainValidation(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateTrain("  ", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank name: %v", err)
	}
}

func TestListVersionsMissingComponent(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.ListVersions("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}
