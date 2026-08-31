package ports

import (
	"context"
	"errors"
	"io"
	"time"

	"claracore-server/internal/platform"
)

var ErrTenantExportTooLarge = errors.New("tenant export exceeds configured size limit")

// TenantExportInput describes one bounded, portable team export. It is not a
// database backup contract: credentials, sessions, embeddings and runtime
// queues are intentionally excluded.
type TenantExportInput struct {
	TenantID    platform.ID
	GeneratedAt time.Time
	MaxBytes    int64
}

type TenantExportResult struct {
	FormatVersion int
	GeneratedAt   time.Time
	RecordCount   int64
	BytesWritten  int64
}

// TenantExporter writes a complete ZIP only when every tenant-scoped query and
// the final manifest succeed. Callers should spool it before sending headers so
// a database failure cannot look like a valid partial download.
type TenantExporter interface {
	Export(context.Context, TenantExportInput, io.Writer) (TenantExportResult, error)
}
