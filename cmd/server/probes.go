package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"claracore-server/internal/adapters/mysql"
)

const readinessTimeout = 3 * time.Second

type probeOptions struct {
	Version         string
	SourceDigest    string
	BinaryDigest    string
	Env             string
	InstanceID      string
	Now             func() time.Time
	NewTraceID      func() string
	Accepting       func() bool
	DrainRetryAfter time.Duration
	DatabaseHealth  func(context.Context) error
	SchemaHealth    func(context.Context) (mysql.SchemaStatus, error)
}

type probeResponse struct {
	OK            bool   `json:"ok"`
	Status        string `json:"status"`
	Version       string `json:"version"`
	SourceDigest  string `json:"sourceDigest"`
	BinaryDigest  string `json:"binaryDigest"`
	Env           string `json:"env"`
	InstanceID    string `json:"instanceId,omitempty"`
	Time          string `json:"time"`
	TraceID       string `json:"traceId"`
	Reason        string `json:"reason,omitempty"`
	MySQL         string `json:"mysql,omitempty"`
	Schema        string `json:"schema,omitempty"`
	SchemaVersion string `json:"schemaVersion,omitempty"`
}

type preflightResponse struct {
	OK            bool   `json:"ok"`
	Status        string `json:"status"`
	Version       string `json:"version"`
	SourceDigest  string `json:"sourceDigest"`
	BinaryDigest  string `json:"binaryDigest"`
	Env           string `json:"env"`
	MySQL         string `json:"mysql"`
	Schema        string `json:"schema"`
	SchemaVersion string `json:"schemaVersion"`
}

type upgradePlanResponse struct {
	OK                   bool   `json:"ok"`
	Status               string `json:"status"`
	Version              string `json:"version"`
	SourceDigest         string `json:"sourceDigest"`
	BinaryDigest         string `json:"binaryDigest"`
	Env                  string `json:"env"`
	MySQL                string `json:"mysql"`
	Schema               string `json:"schema"`
	CurrentSchemaVersion string `json:"currentSchemaVersion,omitempty"`
	TargetSchemaVersion  string `json:"targetSchemaVersion"`
	NextMigration        string `json:"nextMigration,omitempty"`
	Applied              int    `json:"applied"`
	Expected             int    `json:"expected"`
	Pending              int    `json:"pending"`
	RequiresMaintenance  bool   `json:"requiresSchemaMaintenance"`
	RollbackMode         string `json:"rollbackMode"`
}

func writePreflightSuccess(w io.Writer, version, buildSourceDigest, buildBinaryDigest, env string, schema mysql.SchemaStatus) error {
	return json.NewEncoder(w).Encode(preflightResponse{
		OK:            true,
		Status:        "ready",
		Version:       version,
		SourceDigest:  buildSourceDigest,
		BinaryDigest:  buildBinaryDigest,
		Env:           env,
		MySQL:         "reachable",
		Schema:        "current",
		SchemaVersion: schema.LatestVersion,
	})
}

func writeUpgradePlanSuccess(w io.Writer, version, buildSourceDigest, buildBinaryDigest, env string, plan mysql.SchemaPlan) error {
	status, rollback := "current", "binary_only"
	if plan.Pending > 0 {
		status, rollback = "upgrade_required", "restore_required"
	}
	return json.NewEncoder(w).Encode(upgradePlanResponse{
		OK: true, Status: status, Version: version, SourceDigest: buildSourceDigest, BinaryDigest: buildBinaryDigest, Env: env, MySQL: "reachable", Schema: status,
		CurrentSchemaVersion: plan.CurrentVersion, TargetSchemaVersion: plan.TargetVersion,
		NextMigration: plan.NextMigration,
		Applied:       plan.Applied, Expected: plan.Expected, Pending: plan.Pending,
		RequiresMaintenance: plan.Pending > 0, RollbackMode: rollback,
	})
}

// readinessGate 是进程本地的接流量开关。SIGTERM 到达后先关闭它，
// 让负载均衡在真正关 listener 之前有时间把实例从后端池摘掉。
type readinessGate struct {
	accepting atomic.Bool
}

func newReadinessGate() *readinessGate {
	gate := &readinessGate{}
	gate.accepting.Store(true)
	return gate
}

func (g *readinessGate) Accepting() bool { return g != nil && g.accepting.Load() }
func (g *readinessGate) StartDraining()  { g.accepting.Store(false) }

// newLivenessProbe 只回答进程是否还能处理 HTTP。
// 数据库故障不能让编排器反复重启一个本身健康、只是依赖暂时不可用的进程。
func newLivenessProbe(opts probeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeProbe(w, http.StatusOK, probeResponse{
			OK:           true,
			Status:       "alive",
			Version:      opts.Version,
			SourceDigest: opts.SourceDigest,
			BinaryDigest: opts.BinaryDigest,
			Env:          opts.Env,
			InstanceID:   opts.InstanceID,
			Time:         opts.Now().Format(time.RFC3339),
			TraceID:      opts.NewTraceID(),
		})
	}
}

// newReadinessProbe 只有在 MySQL 可达且数据库迁移与当前二进制精确匹配时
// 才允许实例接流量。Redis 与 embedding 都有明确降级语义，不属于硬依赖。
func newReadinessProbe(opts probeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if opts.Accepting != nil && !opts.Accepting() {
			setRetryAfter(w, opts.DrainRetryAfter)
			writeProbe(w, http.StatusServiceUnavailable, probeResponse{
				OK:           false,
				Status:       "draining",
				Version:      opts.Version,
				SourceDigest: opts.SourceDigest,
				BinaryDigest: opts.BinaryDigest,
				Env:          opts.Env,
				InstanceID:   opts.InstanceID,
				Time:         opts.Now().Format(time.RFC3339),
				TraceID:      opts.NewTraceID(),
				Reason:       "shutdown",
			})
			return
		}
		response := probeResponse{
			OK:           false,
			Status:       "not_ready",
			Version:      opts.Version,
			SourceDigest: opts.SourceDigest,
			BinaryDigest: opts.BinaryDigest,
			Env:          opts.Env,
			InstanceID:   opts.InstanceID,
			Time:         opts.Now().Format(time.RFC3339),
			TraceID:      opts.NewTraceID(),
			MySQL:        "unreachable",
			Schema:       "unchecked",
		}

		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if opts.DatabaseHealth == nil || opts.DatabaseHealth(ctx) != nil {
			writeProbe(w, http.StatusServiceUnavailable, response)
			return
		}
		response.MySQL = "reachable"
		if opts.SchemaHealth == nil {
			response.Schema = "not_current"
			writeProbe(w, http.StatusServiceUnavailable, response)
			return
		}
		status, err := opts.SchemaHealth(ctx)
		if err != nil {
			response.Schema = "not_current"
			writeProbe(w, http.StatusServiceUnavailable, response)
			return
		}

		response.OK = true
		response.Status = "ready"
		response.Schema = "current"
		response.SchemaVersion = status.LatestVersion
		writeProbe(w, http.StatusOK, response)
	}
}

// rejectNewRequestsWhileDraining 拒绝 gate 关闭后才到达的业务请求。
// 已经进入下游 handler 的在途请求不受影响；三个探针继续可访问，方便
// 编排器区分“进程仍活着”与“实例已退出流量池”。
func rejectNewRequestsWhileDraining(next http.Handler, accepting func() bool, retryAfter time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepting != nil && !accepting() && !isProbePath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
			setRetryAfter(w, retryAfter)
			http.Error(w, "服务正在退出，请稍后重试", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setRetryAfter(w http.ResponseWriter, window time.Duration) {
	seconds := int64((window + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

func isProbePath(path string) bool {
	return path == "/livez" || path == "/readyz" || path == "/healthz"
}

func writeProbe(w http.ResponseWriter, status int, response probeResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
