package releasetrain

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是语义化版本号 major.minor.patch（只保留数字核心，预发布标签不参与）。
// 组件版本一经登记即不可修改（见 Service.RegisterVersion 的幂等语义）。
type Version struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

// ParseVersion 解析 "1.2.3" 形式的版本号。
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: version %q must be major.minor.patch", ErrInvalidArgument, s)
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%w: version %q has non-numeric or negative component", ErrInvalidArgument, s)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, nil
}

// String 返回 major.minor.patch。
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// Compare 语义化比较：-1 / 0 / 1。
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpInt(v.Major, o.Major)
	case v.Minor != o.Minor:
		return cmpInt(v.Minor, o.Minor)
	default:
		return cmpInt(v.Patch, o.Patch)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// ConstraintOp 是组件声明的版本约束操作符。
type ConstraintOp string

const (
	OpEq ConstraintOp = "="  // 精确等于
	OpGt ConstraintOp = ">"  // 大于
	OpGe ConstraintOp = ">=" // 大于等于
	OpLt ConstraintOp = "<"  // 小于
	OpLe ConstraintOp = "<=" // 小于等于
)

// Constraint 是一个组件版本对另一个组件版本的声明式约束：dep ^op version。
// 例如 Payment 1.2.0 声明 ("order", ">=", 2.0.0)。
type Constraint struct {
	Component string       `json:"component"`
	Op        ConstraintOp `json:"op"`
	Version   Version      `json:"version"`
}

// SatisfiedBy 判断给定版本是否满足约束。
func (c Constraint) SatisfiedBy(v Version) bool {
	cmp := v.Compare(c.Version)
	switch c.Op {
	case OpEq:
		return cmp == 0
	case OpGt:
		return cmp > 0
	case OpGe:
		return cmp >= 0
	case OpLt:
		return cmp < 0
	case OpLe:
		return cmp <= 0
	default:
		return false
	}
}

func (c Constraint) String() string {
	return c.Component + " " + string(c.Op) + " " + c.Version.String()
}

func validOp(op ConstraintOp) bool {
	switch op {
	case OpEq, OpGt, OpGe, OpLt, OpLe:
		return true
	default:
		return false
	}
}
