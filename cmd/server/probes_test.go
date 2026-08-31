package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
)

func TestPreflightSuccessIsMachineReadableAndSecretFree(t *testing.T) {
	var buf bytes.Buffer
	if err := writePreflightSuccess(&buf, "v-test", strings.Repeat("a", 64), strings.Repeat("f", 64), "production", mysql.SchemaStatus{LatestVersion: "0008", Expected: 8, Applied: 8}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("预检输出不是合法 JSON: %v\n%s", err, buf.String())
	}
	if got["ok"] != true || got["status"] != "ready" || got["sourceDigest"] != strings.Repeat("a", 64) || got["binaryDigest"] != strings.Repeat("f", 64) || got["mysql"] != "reachable" || got["schema"] != "current" || got["schemaVersion"] != "0008" {
		t.Fatalf("预检输出不完整: %v", got)
	}
	for _, forbidden := range []string{"password", "host", "database", "dsn"} {
		if strings.Contains(strings.ToLower(buf.String()), forbidden) {
			t.Fatalf("预检输出不能包含连接信息 %q: %s", forbidden, buf.String())
		}
	}
}

func TestUpgradePlanMakesDowntimeAndRollbackBoundaryMachineReadable(t *testing.T) {
	var out bytes.Buffer
	plan := mysql.SchemaPlan{
		CurrentVersion: "0010", TargetVersion: "0011", NextMigration: "0011_example.sql",
		Applied: 10, Expected: 11, Pending: 1,
	}
	if err := writeUpgradePlanSuccess(&out, "v-next", strings.Repeat("b", 64), strings.Repeat("e", 64), "production", plan); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("upgrade plan is not JSON: %v\n%s", err, out.String())
	}
	if got["status"] != "upgrade_required" || got["schema"] != "upgrade_required" ||
		got["requiresSchemaMaintenance"] != true || got["rollbackMode"] != "restore_required" ||
		got["currentSchemaVersion"] != "0010" || got["targetSchemaVersion"] != "0011" ||
		got["nextMigration"] != "0011_example.sql" || got["pending"] != float64(1) || got["sourceDigest"] != strings.Repeat("b", 64) || got["binaryDigest"] != strings.Repeat("e", 64) {
		t.Fatalf("upgrade plan hides an operational boundary: %v", got)
	}

	out.Reset()
	got = map[string]any{}
	if err := writeUpgradePlanSuccess(&out, "v-current", strings.Repeat("c", 64), strings.Repeat("d", 64), "production", mysql.SchemaPlan{
		CurrentVersion: "0010", TargetVersion: "0010", Applied: 10, Expected: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "current" || got["requiresSchemaMaintenance"] != false || got["rollbackMode"] != "binary_only" {
		t.Fatalf("schema-neutral release plan = %v", got)
	}
}

func TestLivenessDoesNotDependOnExternalServices(t *testing.T) {
	dependencyCalls := 0
	handler := newLivenessProbe(probeOptions{
		Version:      "v-test",
		SourceDigest: strings.Repeat("d", 64),
		BinaryDigest: strings.Repeat("e", 64),
		Env:          "production",
		InstanceID:   "ccs-a",
		Now:          func() time.Time { return time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC) },
		NewTraceID:   func() string { return "trace-live" },
		DatabaseHealth: func(context.Context) error {
			dependencyCalls++
			return errors.New("mysql down")
		},
	})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rr.Code != http.StatusOK || dependencyCalls != 0 {
		t.Fatalf("liveness 不应访问依赖: status=%d calls=%d body=%s", rr.Code, dependencyCalls, rr.Body.String())
	}
	assertProbeBody(t, rr, `"ok":true`, `"status":"alive"`, `"version":"v-test"`, `"sourceDigest":"`+strings.Repeat("d", 64)+`"`, `"binaryDigest":"`+strings.Repeat("e", 64)+`"`, `"instanceId":"ccs-a"`, `"traceId":"trace-live"`)
}

func TestReadinessRequiresDatabaseAndCurrentSchema(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		rr := runReadinessProbe(t,
			func(context.Context) error { return nil },
			func(context.Context) (mysql.SchemaStatus, error) {
				return mysql.SchemaStatus{LatestVersion: "0008", Expected: 8, Applied: 8}, nil
			},
		)
		if rr.Code != http.StatusOK {
			t.Fatalf("ready status=%d body=%s", rr.Code, rr.Body.String())
		}
		assertProbeBody(t, rr, `"ok":true`, `"status":"ready"`, `"mysql":"reachable"`, `"schema":"current"`, `"schemaVersion":"0008"`)
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("探针响应不可缓存，得到 %q", got)
		}
	})

	t.Run("database unavailable skips schema", func(t *testing.T) {
		schemaCalls := 0
		rr := runReadinessProbe(t,
			func(context.Context) error { return errors.New("down") },
			func(context.Context) (mysql.SchemaStatus, error) {
				schemaCalls++
				return mysql.SchemaStatus{}, nil
			},
		)
		if rr.Code != http.StatusServiceUnavailable || schemaCalls != 0 {
			t.Fatalf("数据库失败时必须 503 且跳过 schema: status=%d calls=%d body=%s", rr.Code, schemaCalls, rr.Body.String())
		}
		assertProbeBody(t, rr, `"ok":false`, `"status":"not_ready"`, `"mysql":"unreachable"`, `"schema":"unchecked"`)
	})

	t.Run("schema mismatch", func(t *testing.T) {
		rr := runReadinessProbe(t,
			func(context.Context) error { return nil },
			func(context.Context) (mysql.SchemaStatus, error) {
				return mysql.SchemaStatus{}, errors.New("missing migration")
			},
		)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("schema 不匹配必须 503: status=%d body=%s", rr.Code, rr.Body.String())
		}
		assertProbeBody(t, rr, `"mysql":"reachable"`, `"schema":"not_current"`)
		if strings.Contains(rr.Body.String(), "missing migration") {
			t.Fatal("公开探针不能泄露内部迁移错误")
		}
	})

	t.Run("draining skips dependencies", func(t *testing.T) {
		dependencyCalls := 0
		gate := newReadinessGate()
		gate.StartDraining()
		handler := newReadinessProbe(probeOptions{
			Version:         "v-test",
			Env:             "production",
			Now:             func() time.Time { return time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC) },
			NewTraceID:      func() string { return "trace-draining" },
			Accepting:       gate.Accepting,
			DrainRetryAfter: 5 * time.Second,
			DatabaseHealth: func(context.Context) error {
				dependencyCalls++
				return nil
			},
		})
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rr.Code != http.StatusServiceUnavailable || dependencyCalls != 0 {
			t.Fatalf("draining 必须快速返回 503 且不访问依赖: status=%d calls=%d body=%s", rr.Code, dependencyCalls, rr.Body.String())
		}
		assertProbeBody(t, rr, `"status":"draining"`, `"reason":"shutdown"`)
		if rr.Header().Get("Retry-After") != "5" {
			t.Fatalf("readiness 应提示完整摘流窗口，得到 %q", rr.Header().Get("Retry-After"))
		}
	})
}

func TestDrainingRejectsNewBusinessRequestsButKeepsProbes(t *testing.T) {
	gate := newReadinessGate()
	nextCalls := 0
	handler := rejectNewRequestsWhileDraining(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusNoContent)
	}), gate.Accepting, 5*time.Second)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/browse", nil))
	if rr.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("正常状态必须放行业务请求: status=%d calls=%d", rr.Code, nextCalls)
	}

	gate.StartDraining()
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rr.Code != http.StatusServiceUnavailable || nextCalls != 1 || rr.Header().Get("Retry-After") != "5" {
		t.Fatalf("draining 必须拒绝新业务请求: status=%d calls=%d retry=%q", rr.Code, nextCalls, rr.Header().Get("Retry-After"))
	}

	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNoContent {
			t.Errorf("draining 时探针 %s 仍必须可访问: status=%d", path, rr.Code)
		}
	}
}

func runReadinessProbe(
	t *testing.T,
	database func(context.Context) error,
	schema func(context.Context) (mysql.SchemaStatus, error),
) *httptest.ResponseRecorder {
	t.Helper()
	handler := newReadinessProbe(probeOptions{
		Version:        "v-test",
		SourceDigest:   strings.Repeat("e", 64),
		BinaryDigest:   strings.Repeat("f", 64),
		Env:            "production",
		InstanceID:     "ccs-a",
		Now:            func() time.Time { return time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC) },
		NewTraceID:     func() string { return "trace-ready" },
		DatabaseHealth: database,
		SchemaHealth:   schema,
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rr
}

func TestProbeOmitsUnconfiguredInstanceID(t *testing.T) {
	handler := newLivenessProbe(probeOptions{
		Version: "v-test", Env: "development", Now: time.Now,
		NewTraceID: func() string { return "trace" },
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if strings.Contains(rr.Body.String(), "instanceId") {
		t.Fatalf("single-instance deployment without a label must not expose an empty field: %s", rr.Body.String())
	}
}

func assertProbeBody(t *testing.T, rr *httptest.ResponseRecorder, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("探针响应缺少 %s: %s", want, rr.Body.String())
		}
	}
}
