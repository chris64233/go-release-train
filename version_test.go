package releasetrain

import "testing"

func TestParseVersionAndCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.9", "1.1.0", -1},
		{"1.9.9", "2.0.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"10.0.0", "9.0.0", 1},
	}
	for _, c := range cases {
		va, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("parse %s: %v", c.a, err)
		}
		vb, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("parse %s: %v", c.b, err)
		}
		if got := va.Compare(vb); got != c.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if va.String() != c.a {
			t.Errorf("round trip = %q, want %q", va.String(), c.a)
		}
	}
}

func TestParseVersionInvalid(t *testing.T) {
	for _, bad := range []string{"1.2", "1.2.x", "1.-1.0", "", "1.2.3.4"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) expected error", bad)
		}
	}
}

func TestConstraintSatisfiedBy(t *testing.T) {
	v := func(s string) Version {
		parsed, err := ParseVersion(s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	base := v("2.3.4")
	cases := []struct {
		op   ConstraintOp
		rhs  string
		want bool
	}{
		{OpEq, "2.3.4", true},
		{OpEq, "2.3.5", false},
		{OpGt, "2.3.3", true},
		{OpGt, "2.3.4", false},
		{OpGe, "2.3.4", true},
		{OpLt, "3.0.0", true},
		{OpLt, "2.3.4", false},
		{OpLe, "2.3.4", true},
		{OpLe, "2.3.5", true},
		{OpLe, "2.3.3", false},
	}
	for _, c := range cases {
		con := Constraint{Component: "x", Op: c.op, Version: v(c.rhs)}
		if got := con.SatisfiedBy(base); got != c.want {
			t.Errorf("%s %s satisfied by %s = %v, want %v", c.op, c.rhs, base, got, c.want)
		}
	}
}
