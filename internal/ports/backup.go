package ports

import (
	"time"
)

// BackupStatus is the bounded receipt emitted after a backup archive has passed
// companion, SHA-256, gzip, metadata, and freshness verification. It contains
// no database contents, connection details, or credentials.
type BackupStatus struct {
	Database          string
	Archive           string
	CreatedAt         time.Time
	VerifiedAt        time.Time
	MaxAge            time.Duration
	IntegrityVerified bool
	Recoverability    string
}

// BackupStatusReader reads the latest successful verification receipt. A
// missing or invalid receipt is operational evidence and must be surfaced as
// unavailable/stale rather than silently converted into a healthy status.
type BackupStatusReader interface {
	Read() (BackupStatus, error)
}

// RestoreStatus is the bounded receipt emitted after an archive has been
// imported into an isolated MySQL database, every restored table has passed
// CHECK TABLE QUICK, row counts and migrations have been enumerated, and the
// temporary database has been removed. It contains no restored rows, target
// database name, connection details, or credentials.
type RestoreStatus struct {
	Database                 string
	Archive                  string
	ArchiveSHA256            string
	Report                   string
	ReportSHA256             string
	RestoredAt               time.Time
	MaxAge                   time.Duration
	TableCount               int
	MigrationCount           int
	LatestMigration          string
	TechnicalRestoreVerified bool
	SemanticInspection       string
}

// RestoreStatusReader reads the latest successful isolated-restore receipt.
// Missing, invalid, or stale receipts are evidence gaps and must not be
// silently converted into a healthy disaster-recovery state.
type RestoreStatusReader interface {
	Read() (RestoreStatus, error)
}
