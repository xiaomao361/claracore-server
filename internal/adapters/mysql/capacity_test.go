package mysql

import (
	"context"
	"testing"
)

func TestTenantCapacityReaderRejectsInvalidInput(t *testing.T) {
	if _, err := NewTenantCapacityReader(nil).ReadTenantCapacity(context.Background(), "tenant"); err == nil {
		t.Fatal("nil database unexpectedly accepted")
	}
}
