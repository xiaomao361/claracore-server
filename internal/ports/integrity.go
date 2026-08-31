package ports

import "time"

// IntegrityIssue identifies one fixed, content-free database relationship
// rule. It never contains tenant data or entity identifiers.
type IntegrityIssue struct {
	Domain     string
	Rule       string
	Violations int64
}

// IntegrityStatus is the bounded operational receipt produced by the explicit
// database integrity command. Verified means the fixed rule set completed and
// found no violations; a completed check with violations remains a valid
// receipt but is intentionally not verified healthy.
type IntegrityStatus struct {
	Verified      bool
	SchemaVersion string
	CheckedAt     time.Time
	MaxAge        time.Duration
	RulesChecked  int
	Violations    int64
	Issues        []IntegrityIssue
}

type IntegrityStatusReader interface {
	Read() (IntegrityStatus, error)
}
