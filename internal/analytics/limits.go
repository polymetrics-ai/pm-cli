package analytics

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidBounds identifies a declared analytical bound that is absent,
// non-positive, or beyond the child's finite capability.
var ErrInvalidBounds = errors.New("invalid analytical bounds")

// The child enforces one fixed capability. A request may declare smaller
// bounds; it can never raise them. Absent or non-positive bounds are refused
// rather than defaulted, so no request can run with an implicit unlimited
// resource.
const (
	MaximumRelations       = 8
	MaximumColumns         = 64
	MaximumPredicates      = 16
	MaximumOrderings       = 8
	MaximumPredicateValues = 64
	MaximumRows            = 1 << 20
	MaximumBytes           = 64 << 20
	MaximumTempBytes       = 512 << 20
	MaximumMemoryBytes     = 1 << 30
	MaximumRequestBytes    = 64 << 20
	MaximumResponseBytes   = 64 << 20
	MaximumIdentifierBytes = 64
)

// MaximumWallTime bounds one child execution. The supervisor's own wall-time
// kill remains a separate, shorter observation.
const MaximumWallTime = 5 * time.Minute

// BoundAnalytics is the finite resource declaration one analytical request
// must carry. Every field is required and positive; the engine settings and
// the process caps are derived from these values and never exceed the child
// capability above.
type BoundAnalytics struct {
	Relations   int
	Columns     int
	Rows        int64
	Bytes       int64
	TempBytes   int64
	MemoryBytes int64
	WallTime    time.Duration
}

// Validate refuses an absent, non-positive, or over-capability bound.
func (bounds BoundAnalytics) Validate() error {
	switch {
	case bounds.Relations <= 0:
		return fmt.Errorf("%w: the relation bound is absent or non-positive", ErrInvalidBounds)
	case bounds.Relations > MaximumRelations:
		return fmt.Errorf("%w: the relation bound exceeds the child capability of %d", ErrInvalidBounds, MaximumRelations)
	case bounds.Columns <= 0:
		return fmt.Errorf("%w: the column bound is absent or non-positive", ErrInvalidBounds)
	case bounds.Columns > MaximumColumns:
		return fmt.Errorf("%w: the column bound exceeds the child capability of %d", ErrInvalidBounds, MaximumColumns)
	case bounds.Rows <= 0:
		return fmt.Errorf("%w: the row bound is absent or non-positive", ErrInvalidBounds)
	case bounds.Rows > MaximumRows:
		return fmt.Errorf("%w: the row bound exceeds the child capability of %d", ErrInvalidBounds, MaximumRows)
	case bounds.Bytes <= 0:
		return fmt.Errorf("%w: the byte bound is absent or non-positive", ErrInvalidBounds)
	case bounds.Bytes > MaximumBytes:
		return fmt.Errorf("%w: the byte bound exceeds the child capability of %d", ErrInvalidBounds, MaximumBytes)
	case bounds.TempBytes <= 0:
		return fmt.Errorf("%w: the temporary byte bound is absent or non-positive", ErrInvalidBounds)
	case bounds.TempBytes > MaximumTempBytes:
		return fmt.Errorf("%w: the temporary byte bound exceeds the child capability of %d", ErrInvalidBounds, MaximumTempBytes)
	case bounds.MemoryBytes <= 0:
		return fmt.Errorf("%w: the memory byte bound is absent or non-positive", ErrInvalidBounds)
	case bounds.MemoryBytes > MaximumMemoryBytes:
		return fmt.Errorf("%w: the memory byte bound exceeds the child capability of %d", ErrInvalidBounds, MaximumMemoryBytes)
	case bounds.WallTime <= 0:
		return fmt.Errorf("%w: the wall-time bound is absent or non-positive", ErrInvalidBounds)
	case bounds.WallTime > MaximumWallTime:
		return fmt.Errorf("%w: the wall-time bound exceeds the child capability of %s", ErrInvalidBounds, MaximumWallTime)
	}
	return nil
}

// memoryLimitSetting converts the declared memory bound into the engine
// setting DuckDB accepts, never rounding below the declared bound.
func (bounds BoundAnalytics) memoryLimitSetting() string {
	mebibytes := (bounds.MemoryBytes + (1 << 20) - 1) >> 20
	if mebibytes < 1 {
		mebibytes = 1
	}
	return fmt.Sprintf("%dMB", mebibytes)
}

// tempLimitSetting converts the declared temporary bound into the engine
// setting DuckDB accepts, never rounding below the declared bound.
func (bounds BoundAnalytics) tempLimitSetting() string {
	mebibytes := (bounds.TempBytes + (1 << 20) - 1) >> 20
	if mebibytes < 1 {
		mebibytes = 1
	}
	return fmt.Sprintf("%dMB", mebibytes)
}
