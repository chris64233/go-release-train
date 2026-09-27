package releasetrain

import (
	"errors"
	"testing"
)

// fakeRegistry 内存版 versionLookup。
type fakeRegistry struct {
	data map[string]map[string][]Constraint
}

func (f *fakeRegistry) register(component, version string, cs ...Constraint) {
	if f.data == nil {
		f.data = map[string]map[string][]Constraint{}
	}
	if f.data[component] == nil {
		f.data[component] = map[string][]Constraint{}
	}
	f.data[component][version] = cs
}

func (f *fakeRegistry) constraintsOf(component string, v Version) ([]Constraint, bool) {
	cs, ok := f.data[component][v.String()]
	return cs, ok
}

func mustSnap(t *testing.T, pairs ...string) map[string]Version {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatal("pairs must be component,version pairs")
	}
	snap := map[string]Version{}
	for i := 0; i < len(pairs); i += 2 {
		v, err := ParseVersion(pairs[i+1])
		if err != nil {
			t.Fatal(err)
		}
		snap[pairs[i]] = v
	}
	return snap
}

func TestValidateSnapshotOK(t *testing.T) {
	reg := &fakeRegistry{}
	// order 无依赖；payment 依赖 order >=2.0.0；gateway 依赖两者。
	reg.register("order", "2.1.0")
	reg.register("payment", "1.0.0",
		Constraint{Component: "order", Op: OpGe, Version: Version{2, 0, 0}})
	reg.register("gateway", "3.0.0",
		Constraint{Component: "order", Op: OpGe, Version: Version{2, 0, 0}},
		Constraint{Component: "payment", Op: OpEq, Version: Version{1, 0, 0}})

	snap := mustSnap(t, "order", "2.1.0", "payment", "1.0.0", "gateway", "3.0.0")
	if p := validateSnapshot(snap, reg); len(p) != 0 {
		t.Fatalf("expected clean snapshot, got problems: %v", p)
	}
}

func TestValidateSnapshotUnsatisfiedConstraint(t *testing.T) {
	reg := &fakeRegistry{}
	reg.register("order", "1.9.0")
	reg.register("payment", "1.0.0",
		Constraint{Component: "order", Op: OpGe, Version: Version{2, 0, 0}})

	snap := mustSnap(t, "order", "1.9.0", "payment", "1.0.0")
	p := validateSnapshot(snap, reg)
	if len(p) != 1 {
		t.Fatalf("want 1 problem, got %v", p)
	}
}

func TestValidateSnapshotMissingTarget(t *testing.T) {
	reg := &fakeRegistry{}
	reg.register("payment", "1.0.0",
		Constraint{Component: "order", Op: OpGe, Version: Version{2, 0, 0}})
	snap := mustSnap(t, "payment", "1.0.0")
	p := validateSnapshot(snap, reg)
	if len(p) != 1 {
		t.Fatalf("want 1 problem (missing dep), got %v", p)
	}
}

func TestValidateSnapshotUnregisteredVersion(t *testing.T) {
	reg := &fakeRegistry{}
	reg.register("order", "2.0.0")
	snap := mustSnap(t, "order", "9.9.9")
	p := validateSnapshot(snap, reg)
	if len(p) != 1 {
		t.Fatalf("want 1 problem (unregistered), got %v", p)
	}
}

func TestValidateSnapshotCycle(t *testing.T) {
	reg := &fakeRegistry{}
	// a -> b -> c -> a，另有独立组件 d。
	reg.register("a", "1.0.0", Constraint{Component: "b", Op: OpEq, Version: Version{1, 0, 0}})
	reg.register("b", "1.0.0", Constraint{Component: "c", Op: OpEq, Version: Version{1, 0, 0}})
	reg.register("c", "1.0.0", Constraint{Component: "a", Op: OpEq, Version: Version{1, 0, 0}})
	reg.register("d", "1.0.0")

	snap := mustSnap(t, "a", "1.0.0", "b", "1.0.0", "c", "1.0.0", "d", "1.0.0")
	p := validateSnapshot(snap, reg)
	if len(p) != 1 {
		t.Fatalf("want exactly 1 cycle problem, got %v", p)
	}
}

func TestValidateSnapshotSelfCycle(t *testing.T) {
	reg := &fakeRegistry{}
	reg.register("a", "1.0.0", Constraint{Component: "a", Op: OpEq, Version: Version{1, 0, 0}})
	snap := mustSnap(t, "a", "1.0.0")
	if p := validateSnapshot(snap, reg); len(p) != 1 {
		t.Fatalf("want self-cycle problem, got %v", p)
	}
}

func TestValidateSnapshotCollectsMultipleProblems(t *testing.T) {
	reg := &fakeRegistry{}
	// 一个约束不满足 + 一个环，两类问题必须同时返回，不能遇到第一个就停。
	reg.register("a", "1.0.0",
		Constraint{Component: "b", Op: OpEq, Version: Version{2, 0, 0}},
		Constraint{Component: "c", Op: OpEq, Version: Version{1, 0, 0}})
	reg.register("b", "1.0.0")
	reg.register("c", "1.0.0", Constraint{Component: "a", Op: OpEq, Version: Version{1, 0, 0}})

	snap := mustSnap(t, "a", "1.0.0", "b", "1.0.0", "c", "1.0.0")
	p := validateSnapshot(snap, reg)
	if len(p) < 2 {
		t.Fatalf("want at least 2 problems (unsatisfied + cycle), got %v", p)
	}
}

func TestDependencyErrorIs(t *testing.T) {
	err := &DependencyError{Problems: []string{"x"}}
	if !errors.Is(err, ErrDependency) {
		t.Fatal("DependencyError should match ErrDependency")
	}
}
