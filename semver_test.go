package releasetrain

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1.2.3", "1.2.3", true},
		{"0.0.0", "0.0.0", true},
		{"1.2.3-rc.1", "1.2.3-rc.1", true},
		{"1.2", "", false},
		{"1.2.x", "", false},
		{"01.2.3", "", false},
		{"1.2.3-", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		v, err := ParseVersion(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("ParseVersion(%q) unexpected error: %v", c.in, err)
				continue
			}
			if got := v.String(); got != c.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("ParseVersion(%q) expected error, got %v", c.in, v)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "2.0.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-1", "1.0.0-2", -1},
		{"1.0.0-1", "1.0.0-alpha", -1}, // 数字标识符低于字母标识符
		{"1.0.1", "1.0.0", 1},
	}
	for _, c := range cases {
		va, _ := ParseVersion(c.a)
		vb, _ := ParseVersion(c.b)
		if got := va.Compare(vb); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestConstraintSatisfies(t *testing.T) {
	cases := []struct {
		con string
		v   string
		ok  bool
	}{
		{"=1.0.0", "1.0.0", true},
		{"=1.0.0", "1.0.1", false},
		{"1.0.0", "1.0.0", true}, // 裸版本即精确匹配
		{"!=1.0.0", "1.0.1", true},
		{">=1.2.0", "1.2.0", true},
		{">=1.2.0", "1.1.9", false},
		{"<2.0.0", "1.9.9", true},
		{"<2.0.0", "2.0.0", false},
		{">1.0.0", "1.0.0", false},
		{"^1.2.3", "1.9.0", true},
		{"^1.2.3", "2.0.0", false},
		{"^1.2.3", "1.2.2", false},
		{"^0.2.3", "0.2.9", true},
		{"^0.2.3", "0.3.0", false},
		{"^0.0.3", "0.0.3", true},
		{"^0.0.3", "0.0.4", false},
		{"~1.2.3", "1.2.9", true},
		{"~1.2.3", "1.3.0", false},
		{">=1.0.0,<2.0.0", "1.5.0", true},
		{">=1.0.0,<2.0.0", "2.0.0", false},
	}
	for _, c := range cases {
		list, err := parseConstraintList(c.con)
		if err != nil {
			t.Fatalf("parse %q: %v", c.con, err)
		}
		v, _ := ParseVersion(c.v)
		got := true
		for _, con := range list {
			if !con.Satisfies(v) {
				got = false
			}
		}
		if got != c.ok {
			t.Errorf("%q satisfies %q = %v, want %v", c.con, c.v, got, c.ok)
		}
	}
}
