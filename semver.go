package releasetrain

import (
	"strconv"
	"strings"
)

// Version 是一个语义化版本号：MAJOR.MINOR.PATCH，可带预发布标识。
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // 形如 "rc.1"，可为空；带预发布标识的版本低于同号正式版
}

// ParseVersion 解析 "1.2.3" 或 "1.2.3-rc.1"。
func ParseVersion(s string) (Version, error) {
	var v Version
	s = strings.TrimSpace(s)
	if s == "" {
		return v, fail(KindValidation, "parseVersion", "empty version")
	}
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core = s[:i]
		v.Prerelease = s[i+1:]
		if v.Prerelease == "" {
			return v, fail(KindValidation, "parseVersion", "empty prerelease in %q", s)
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, fail(KindValidation, "parseVersion", "version %q must be MAJOR.MINOR.PATCH", s)
	}
	nums := [3]*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return v, fail(KindValidation, "parseVersion", "invalid numeric component %q in %q", p, s)
		}
		*nums[i] = n
	}
	return v, nil
}

// String 还原规范字符串。
func (v Version) String() string {
	s := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// Compare 语义化版本比较：-1 / 0 / 1。预发布按 semver 规则（点分标识符，数字按数值）。
func (v Version) Compare(o Version) int {
	if c := intCmp(v.Major, o.Major); c != 0 {
		return c
	}
	if c := intCmp(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := intCmp(v.Patch, o.Patch); c != 0 {
		return c
	}
	return comparePrerelease(v.Prerelease, o.Prerelease)
}

func intCmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePrerelease：无预发布（正式版）高于有预发布；否则按点分标识符比较。
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1 // 1.0.0 > 1.0.0-rc
	}
	if b == "" {
		return -1
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		xn, xErr := strconv.Atoi(x)
		yn, yErr := strconv.Atoi(y)
		switch {
		case xErr == nil && yErr == nil:
			if c := intCmp(xn, yn); c != 0 {
				return c
			}
		case xErr == nil && yErr != nil:
			return -1 // 数字标识符低于字母标识符
		case xErr != nil && yErr == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return intCmp(len(as), len(bs))
}

// Constraint 是单个版本约束，例如 ">=1.2.0"、"=1.0.0"、"<2.0.0"、"^1.2.3"、"~1.2.0"。
//
// 支持的操作符：=, !=, >, >=, <, <=, ^（兼容版本：锁定主版本，0.x 锁定次版本）,
// ~（次版本兼容：>=x.y.0, <x.(y+1).0）。
type Constraint struct {
	raw  string
	op   string
	base Version
}

// ParseConstraint 解析单个比较表达式。
func ParseConstraint(s string) (Constraint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Constraint{}, fail(KindValidation, "parseConstraint", "empty constraint")
	}
	op := ""
	for _, prefix := range []string{">=", "<=", "!=", "=", ">", "<", "^", "~"} {
		if strings.HasPrefix(s, prefix) {
			op = prefix
			s = strings.TrimSpace(s[len(prefix):])
			break
		}
	}
	if op == "" { // 裸版本号按精确匹配处理
		op = "="
	}
	v, err := ParseVersion(s)
	if err != nil {
		return Constraint{}, fail(KindValidation, "parseConstraint", "invalid constraint: %v", err)
	}
	return Constraint{raw: op + s, op: op, base: v}, nil
}

// String 返回约束的规范文本。
func (c Constraint) String() string { return c.raw }

// Satisfies 判断给定版本是否满足约束。
func (c Constraint) Satisfies(v Version) bool {
	cmp := v.Compare(c.base)
	switch c.op {
	case "=":
		return cmp == 0
	case "!=":
		return cmp != 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case "^":
		// ^1.2.3 := >=1.2.3 <2.0.0；^0.2.3 := >=0.2.3 <0.3.0；^0.0.3 := =0.0.3
		if cmp < 0 {
			return false
		}
		if c.base.Major > 0 {
			return v.Major == c.base.Major
		}
		if c.base.Minor > 0 {
			return v.Major == 0 && v.Minor == c.base.Minor
		}
		return v.Major == 0 && v.Minor == 0 && v.Patch == c.base.Patch
	case "~":
		// ~1.2.3 := >=1.2.3 <1.3.0
		return cmp >= 0 && v.Major == c.base.Major && v.Minor == c.base.Minor
	}
	return false
}
