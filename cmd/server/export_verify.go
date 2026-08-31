package main

import (
	"encoding/json"
	"io"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/portableexport"
	"claracore-server/internal/teamimport"
)

// writeExportVerification remains independent of runtime configuration and
// external services so the packaged server binary is also an offline verifier.
func writeExportVerification(w io.Writer, result portableexport.Verification) error {
	return json.NewEncoder(w).Encode(result)
}

func writeImportPlan(w io.Writer, plan teamimport.Plan) error {
	return json.NewEncoder(w).Encode(plan)
}

func writeImportResult(w io.Writer, result mysql.TenantImportResult) error {
	return json.NewEncoder(w).Encode(result)
}

func writeImportCheckResult(w io.Writer, result mysql.TenantImportCheckResult) error {
	return json.NewEncoder(w).Encode(result)
}

func writeDataIntegrityResult(w io.Writer, result mysql.DataIntegrityResult) error {
	return json.NewEncoder(w).Encode(result)
}
