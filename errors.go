package releasetrain

import "errors"

// 错误分类。调用方用 errors.Is 判断类别：
//   - ErrInvalidArgument   请求本身不合法
//   - ErrNotFound          聚合或资源不存在
//   - ErrDependency        依赖图问题：有环、约束不满足、候选/版本缺失
//   - ErrApproval          审批问题：无资格、角色不满足、审批未齐
//   - ErrStateConflict     状态机冲突（如冻结后再编辑、已放行后取消）
//   - ErrVersionConflict   乐观版本条件不匹配（expectedVersion 过期）
//   - ErrIdempotencyConflict 同一幂等键携带了不同的请求内容，或不可变版本被改写
var (
	ErrInvalidArgument     = errors.New("invalid argument")
	ErrNotFound            = errors.New("not found")
	ErrDependency          = errors.New("dependency conflict")
	ErrApproval            = errors.New("approval rejected")
	ErrStateConflict       = errors.New("state conflict")
	ErrVersionConflict     = errors.New("version conflict")
	ErrIdempotencyConflict = errors.New("idempotency conflict")
)

// DependencyError 携带冻结校验时发现的全部问题。
// 冻结是原子操作：只要 Problems 非空，整个冻结失败，不会留下任何部分结果。
type DependencyError struct {
	Problems []string
}

func (e *DependencyError) Error() string {
	return "dependency conflict: " + joinProblems(e.Problems)
}

// Is 让 *DependencyError 同时匹配 ErrDependency，
// 调用方既能 errors.Is(err, ErrDependency) 分类，也能拿到详细问题列表。
func (e *DependencyError) Is(target error) bool { return target == ErrDependency }

func joinProblems(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
