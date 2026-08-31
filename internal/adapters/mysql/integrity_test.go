package mysql

import (
	"context"
	"testing"
)

func TestDataIntegrityCheckerRejectsInvalidDependencies(t *testing.T) {
	t.Parallel()

	for name, checker := range map[string]*DataIntegrityChecker{
		"nil checker":   nil,
		"nil database":  NewDataIntegrityChecker(nil, "0010"),
		"empty version": &DataIntegrityChecker{},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := checker.Check(context.Background()); err == nil {
				t.Fatal("invalid checker unexpectedly succeeded")
			}
		})
	}
}
