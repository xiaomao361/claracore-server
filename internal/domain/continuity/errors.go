package continuity

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidArgument = errors.New("continuity: 参数非法")
	ErrNotFound        = errors.New("continuity: Shared Line 不存在")
	ErrVersionConflict = errors.New("continuity: 版本冲突")
	ErrTenantMismatch  = errors.New("continuity: 租户不一致")
	ErrLineIDRequired  = errors.New("continuity: 需要明确 lineId")
)

type VersionConflictError struct{ Current int }

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("%v（当前版本 %d）", ErrVersionConflict, e.Current)
}
func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

type LineIDRequiredError struct{ Candidates []LineSummary }

func (e *LineIDRequiredError) Error() string {
	return fmt.Sprintf("%v（候选 %d 条）", ErrLineIDRequired, len(e.Candidates))
}
func (e *LineIDRequiredError) Unwrap() error { return ErrLineIDRequired }
