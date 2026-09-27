package releasetrain

import (
	"errors"
	"fmt"
)

// 错误类别，调用方可用 Kind(err) 或 errors.Is 区分依赖、审批、状态、版本、幂等等冲突。
const (
	KindValidation  = "validation"
	KindNotFound    = "not_found"
	KindDependency  = "dependency"
	KindApproval    = "approval"
	KindState       = "state"
	KindVersion     = "version"
	KindIdempotency = "idempotency"
)

// 各类别的哨兵错误，便于 errors.Is 判定。
var (
	ErrValidation          = &Error{Kind: KindValidation, Msg: "validation failed"}
	ErrNotFound            = &Error{Kind: KindNotFound, Msg: "not found"}
	ErrDependencyConflict  = &Error{Kind: KindDependency, Msg: "dependency conflict"}
	ErrApprovalConflict    = &Error{Kind: KindApproval, Msg: "approval conflict"}
	ErrStateConflict       = &Error{Kind: KindState, Msg: "invalid state"}
	ErrVersionConflict     = &Error{Kind: KindVersion, Msg: "revision conflict"}
	ErrIdempotencyConflict = &Error{Kind: KindIdempotency, Msg: "idempotency conflict"}
)

// Error 是服务对外返回的结构化错误。
type Error struct {
	Kind    string // 错误类别，见上方常量
	Op      string // 发生错误的操作名
	Msg     string // 人类可读的细节
	Current int64  // 版本冲突时，服务端当前修订号（0 表示不适用）
	err     error  // 对应类别的哨兵错误
}

func (e *Error) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("releasetrain: %s: %s", e.Op, e.Msg)
	}
	return "releasetrain: " + e.Msg
}

// Unwrap 让 errors.Is(err, ErrXxxConflict) 生效。
// 注意哨兵自身的 Unwrap 必须为 nil（终结链），否则按 Kind 回到自身会无限递归。
func (e *Error) Unwrap() error {
	return e.err
}

func sentinelFor(kind string) error {
	switch kind {
	case KindValidation:
		return ErrValidation
	case KindNotFound:
		return ErrNotFound
	case KindDependency:
		return ErrDependencyConflict
	case KindApproval:
		return ErrApprovalConflict
	case KindState:
		return ErrStateConflict
	case KindVersion:
		return ErrVersionConflict
	case KindIdempotency:
		return ErrIdempotencyConflict
	}
	return nil
}

func fail(kind, op, format string, args ...any) error {
	return &Error{
		Kind: kind,
		Op:   op,
		Msg:  fmt.Sprintf(format, args...),
		err:  sentinelFor(kind),
	}
}

// Kind 返回错误类别，非本包错误返回空串。
func Kind(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
