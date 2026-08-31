package mysql

import (
	"slices"
	"testing"

	"claracore-server/internal/portableexport"
)

func TestTenantExportDatasetsMatchPortableFormat(t *testing.T) {
	got := make([]string, 0, len(tenantExportDatasets))
	for _, dataset := range tenantExportDatasets {
		got = append(got, dataset.name)
	}
	if want := portableexport.DataFiles(); !slices.Equal(got, want) {
		t.Fatalf("export datasets = %v, portable format requires %v", got, want)
	}
}
