package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"claracore-server/internal/domain/external"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/ports"
)

type externalSourceView struct {
	Name, System, Status, Detail, LastSuccess string
	Attention                                 bool
}

type systemPageData struct {
	pageData
	Environment           string
	SourceDigest          string
	SourceDigestShort     string
	BinaryDigest          string
	BinaryDigestShort     string
	SchemaVersion         string
	SchemaStatus          string
	Uptime                string
	ShutdownPolicy        string
	DatabaseStatus        string
	DatabaseAttention     bool
	DatabasePoolAvailable bool
	DatabasePoolUsage     string
	DatabasePoolStatus    string
	DatabasePoolDetail    string
	RedisStatus           string
	EmbeddingProvider     string
	MySQLTransport        string
	RedisTransport        string
	EmbeddingTransport    string
	ExternalTransport     string
	Embedding             knowledge.EmbeddingHealth
	CacheEnabled          bool
	CacheHits             uint64
	CacheMisses           uint64
	LoginProtection       string
	MCPProtection         string
	MetricsStatus         string
	RuntimeShape          string
	BackupStatus          string
	BackupDetail          string
	BackupRecovery        string
	BackupAttention       bool
	RestoreStatus         string
	RestoreDetail         string
	RestoreScope          string
	RestoreAttention      bool
	IntegrityStatus       string
	IntegrityDetail       string
	IntegrityAttention    bool
	CapacityStatus        string
	CapacityDetail        string
	CapacityMeasuredAt    string
	CapacityAttention     bool
	CapacityAvailable     bool
	Capacity              ports.TenantCapacitySnapshot
	CapacityTotal         string
	KnowledgeCapacity     string
	ContinuityCapacity    string
	EmbeddingCapacity     string
	ExternalCapacity      string
	AuditCapacity         string
	HTTPStatus            string
	HTTPDetail            string
	HTTPAttention         bool
	ExternalStatus        string
	ExternalDetail        string
	ExternalAttention     bool
	ExternalSources       []externalSourceView
}

func integrityIssueSummary(issues []ports.IntegrityIssue) string {
	if len(issues) == 0 {
		return "无规则明细"
	}
	limit := len(issues)
	if limit > 3 {
		limit = 3
	}
	names := make([]string, 0, limit)
	for _, issue := range issues[:limit] {
		names = append(names, issue.Domain+"."+issue.Rule)
	}
	result := strings.Join(names, "、")
	if len(issues) > limit {
		result += fmt.Sprintf(" 等 %d 类", len(issues))
	}
	return result
}

func (h *Host) handleSystem(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := systemPageData{
		pageData:           h.page(r, "系统状态", "system"),
		Environment:        h.opts.Env,
		SourceDigest:       h.opts.SourceDigest,
		BinaryDigest:       h.opts.BinaryDigest,
		SchemaVersion:      h.opts.SchemaVersion,
		SchemaStatus:       "未提供检查器",
		Uptime:             "未知",
		ShutdownPolicy:     "收到 SIGTERM 后立即关闭 listener",
		DatabaseStatus:     "未提供检查器",
		RedisStatus:        "未配置；登录与 MCP 限流使用进程内计数，数据库会话仍可即时吊销",
		EmbeddingProvider:  h.opts.EmbeddingProvider,
		MySQLTransport:     h.opts.MySQLTransport,
		RedisTransport:     h.opts.RedisTransport,
		EmbeddingTransport: h.opts.EmbeddingTransport,
		ExternalTransport:  h.opts.ExternalTransport,
		LoginProtection:    "未装配；请检查服务配置与启动日志",
		MCPProtection:      "未装配；请检查服务配置与启动日志",
		MetricsStatus:      "未启用；只能依赖探针、日志和人工系统页",
		RuntimeShape:       "单实例完整；多实例需配置 Redis 共享登录/MCP 计数与索引协调",
		BackupStatus:       "未接入回执",
		BackupDetail:       "未配置 CCS_BACKUP_STATUS_FILE，系统无法判断最近备份是否按期完成",
		BackupRecovery:     "没有可用的完整性证据；可恢复性也尚未证明",
		BackupAttention:    true,
		RestoreStatus:      "未接入回执",
		RestoreDetail:      "未配置 CCS_RESTORE_STATUS_FILE，系统无法判断隔离恢复演练是否仍在有效期内",
		RestoreScope:       "没有技术恢复证据；代表性 Memory、Card 与 Shared Line 的人工抽查也尚未证明",
		RestoreAttention:   true,
		IntegrityStatus:    "未接入回执",
		IntegrityDetail:    "未配置 CCS_INTEGRITY_STATUS_FILE，系统无法判断关键跨表关系是否按期检查",
		IntegrityAttention: true,
		CapacityStatus:     "未装配",
		CapacityDetail:     "无法读取租户容量快照；当前没有可信的存储配额依据",
		CapacityAttention:  true,
		HTTPStatus:         "未提供统计器",
		HTTPDetail:         "无法判断最近请求量、服务端错误或延迟",
		ExternalStatus:     "未配置",
		ExternalDetail:     "没有启用 Atlas 活源；不影响 Memory、Card 与 Shared Line",
	}
	if data.MySQLTransport == "" {
		data.MySQLTransport = "未提供传输配置摘要"
	}
	if data.RedisTransport == "" {
		data.RedisTransport = "未配置"
	}
	if data.EmbeddingTransport == "" {
		data.EmbeddingTransport = "未配置"
	}
	if data.ExternalTransport == "" {
		data.ExternalTransport = "未配置"
	}
	if data.SchemaVersion == "" {
		data.SchemaVersion = "unknown"
	}
	if data.SourceDigest == "" {
		data.SourceDigest = "unknown"
	}
	data.SourceDigestShort = data.SourceDigest
	if len(data.SourceDigestShort) > 12 {
		data.SourceDigestShort = data.SourceDigestShort[:12]
	}
	if data.BinaryDigest == "" {
		data.BinaryDigest = "unknown"
	}
	data.BinaryDigestShort = data.BinaryDigest
	if len(data.BinaryDigestShort) > 12 {
		data.BinaryDigestShort = data.BinaryDigestShort[:12]
	}
	if !h.opts.StartedAt.IsZero() {
		if uptime := h.opts.Now().Sub(h.opts.StartedAt); uptime >= 0 {
			data.Uptime = displayUptime(uptime)
		}
	}
	if h.opts.ShutdownDrain > 0 {
		data.ShutdownPolicy = fmt.Sprintf(
			"先停止 readiness 与新业务请求，等待 %s 摘流后关闭 listener",
			displayDuration(h.opts.ShutdownDrain),
		)
	}
	if h.opts.LoginRateLimiter != nil {
		data.LoginProtection = fmt.Sprintf(
			"已启用；IP 与账号组合 %d 次/%s，同 IP %d 次/%s",
			h.opts.LoginRateLimit, displayDuration(h.opts.LoginRateWindow),
			h.opts.LoginRateLimit*6, displayDuration(h.opts.LoginRateWindow),
		)
	}
	if h.opts.MCPRateLimit > 0 && h.opts.MCPTenantRateLimit >= h.opts.MCPRateLimit && h.opts.MCPRateWindow > 0 {
		data.MCPProtection = fmt.Sprintf(
			"每 agent %d 次/%s、每租户合计 %d 次/%s；token 与新增 agent 都不能绕过对应预算，来源认证预算为 agent 的 6 倍",
			h.opts.MCPRateLimit, displayDuration(h.opts.MCPRateWindow),
			h.opts.MCPTenantRateLimit, displayDuration(h.opts.MCPRateWindow),
		)
	}
	if h.opts.MetricsAddr != "" {
		data.MetricsStatus = fmt.Sprintf("已在本机 %s 提供 Prometheus 文本指标；公网 nginx 明确拒绝 /metrics", h.opts.MetricsAddr)
	}
	if data.Environment == "" {
		data.Environment = "unknown"
	}
	if h.opts.HTTPRequestStats != nil {
		stats := h.opts.HTTPRequestStats()
		if stats.Available {
			window := displayDuration(stats.Window)
			data.HTTPDetail = fmt.Sprintf(
				"本实例%s内完成 %d · 4xx %d · 5xx %d · panic %d · P95上界 %s · 平均 %s · 当前在途 %d",
				window, stats.Requests, stats.ClientErrors, stats.ServerErrors, stats.Panics,
				displayHTTPP95(stats.P95Latency, stats.P95Overflow), displayHTTPLatency(stats.AverageLatency), stats.InFlight,
			)
			switch {
			case stats.Panics > 0:
				data.HTTPStatus = "发现 panic"
				data.HTTPAttention = true
			case stats.ServerErrors > 0:
				data.HTTPStatus = "存在服务端错误"
				data.HTTPAttention = true
			case stats.Requests == 0:
				data.HTTPStatus = "暂无业务请求"
			default:
				data.HTTPStatus = "请求正常"
			}
		}
	}
	databaseHealthy := false
	if h.opts.DatabaseHealth != nil {
		if err := h.opts.DatabaseHealth(r.Context()); err != nil {
			data.DatabaseStatus = "连接失败"
			data.Error = "数据库当前不可达，请先检查服务日志与数据库连接。"
			h.logger.Error("系统页数据库健康检查失败", "error", err)
		} else {
			data.DatabaseStatus = "连接正常"
			databaseHealthy = true
		}
	}
	if databaseHealthy && h.opts.SchemaHealth != nil {
		if current, err := h.opts.SchemaHealth(r.Context()); err != nil {
			data.SchemaStatus = "与当前二进制不一致，实例不能安全接收流量"
			data.DatabaseAttention = true
			if data.Error == "" {
				data.Error = "数据库迁移状态异常，请检查 readiness 与服务日志。"
			}
			h.logger.Error("系统页数据库迁移检查失败", "error", err)
		} else {
			data.SchemaVersion = current
			data.SchemaStatus = "与当前二进制一致"
		}
	}
	if h.opts.DatabasePoolStats != nil {
		stats := h.opts.DatabasePoolStats()
		data.DatabasePoolAvailable = true
		data.DatabasePoolUsage = fmt.Sprintf("%d / %d", stats.InUse, stats.MaxOpenConnections)
		closed := stats.MaxIdleClosed + stats.MaxIdleTimeClosed + stats.MaxLifetimeClosed
		data.DatabasePoolDetail = fmt.Sprintf(
			"打开 %d · 空闲 %d · 累计等待 %d 次 / %s · 累计回收 %d",
			stats.OpenConnections, stats.Idle, stats.WaitCount,
			displayDuration(stats.WaitDuration), closed,
		)
		switch {
		case stats.MaxOpenConnections <= 0:
			data.DatabasePoolStatus = "未设置连接上限"
		case stats.InUse >= stats.MaxOpenConnections:
			data.DatabasePoolStatus = "已到上限"
			data.DatabaseAttention = true
		case float64(stats.InUse)/float64(stats.MaxOpenConnections) >= 0.8:
			data.DatabasePoolStatus = "接近上限"
			data.DatabaseAttention = true
		default:
			data.DatabasePoolStatus = "容量正常"
		}
	}
	if data.DatabaseStatus != "连接正常" {
		data.DatabaseAttention = true
	}
	if h.opts.RedisConfigured {
		data.RedisStatus = "已配置；跨实例登录与 MCP 限流已装配，连通性检查器未提供"
		data.RuntimeShape = "多实例基础协调已装配；仍需负载均衡与滚动发布实机验收"
		if h.opts.RedisHealth != nil {
			if err := h.opts.RedisHealth(r.Context()); err != nil {
				data.RedisStatus = "已配置但当前不可达；登录与 MCP 限流退回进程内保护，索引租约按幂等写回降级"
				data.RuntimeShape = "多实例协调当前降级；各实例独立保护，核心读写继续可用"
				if data.Notice == "" {
					data.Notice = "Redis 当前不可用；登录与 MCP 调用仍受单实例保护，核心读写不受影响。"
				}
				h.logger.Warn("系统页 Redis 健康检查失败")
			} else {
				data.RedisStatus = "连接正常；跨实例登录与 MCP 限流已启用"
				if h.opts.EmbeddingProvider != "" && h.opts.EmbeddingProvider != "disabled" {
					data.RedisStatus += "，embedding 缓存与索引租约已启用"
				}
			}
		}
	}
	if health, err := h.know.EmbeddingHealth(r.Context(), p.TenantID, 5); err != nil {
		data.Notice = "Embedding 索引状态暂时不可用。"
		h.logger.Warn("系统页读取 Embedding 状态失败", "error", err)
	} else {
		data.Embedding = health
	}
	if h.opts.EmbeddingCacheStats != nil {
		data.CacheHits, data.CacheMisses, data.CacheEnabled = h.opts.EmbeddingCacheStats()
	}
	if h.opts.BackupStatus != nil {
		status, err := h.opts.BackupStatus.Read()
		if err != nil {
			data.BackupStatus = "无有效回执"
			data.BackupDetail = "回执缺失、不可读或格式无效；请检查备份 timer、oneshot 与服务日志"
			data.BackupRecovery = "无法确认最新归档的 SHA-256 与 gzip 完整性；可恢复性尚未证明"
			h.logger.Warn("系统页读取备份状态回执失败", "error", err)
		} else if !status.IntegrityVerified || status.Recoverability != "not_proven" || status.MaxAge <= 0 {
			data.BackupStatus = "回执语义无效"
			data.BackupDetail = "回执没有同时证明 SHA-256、gzip、metadata 完整性与有效年龄预算"
			data.BackupRecovery = "不能把不完整或声称已可恢复的回执展示为健康；仍需隔离恢复演练"
			h.logger.Warn("系统页拒绝语义无效的备份状态回执")
		} else {
			now := h.opts.Now()
			age := now.Sub(status.CreatedAt)
			verifiedAgo := now.Sub(status.VerifiedAt)
			if age < -5*time.Minute || verifiedAgo < -5*time.Minute {
				data.BackupStatus = "回执时间异常"
				data.BackupDetail = "备份或验证时间显著晚于当前实例时间，请检查主机 UTC 时钟"
				data.BackupRecovery = "时间异常时不能用该回执证明 RPO；可恢复性尚未证明"
			} else {
				if age < 0 {
					age = 0
				}
				if verifiedAgo < 0 {
					verifiedAgo = 0
				}
				data.BackupDetail = fmt.Sprintf(
					"%s · 创建于 UTC %s · 年龄 %s / 上限 %s · 最后验证于 %s 前",
					status.Archive, status.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
					displayUptime(age), displayDuration(status.MaxAge), displayUptime(verifiedAgo),
				)
				data.BackupRecovery = "SHA-256、gzip 与 metadata 完整性已验证；可恢复性尚未证明，仍需隔离恢复演练"
				if age > status.MaxAge {
					data.BackupStatus = "最新备份已过期"
				} else {
					data.BackupStatus = "最近备份按期完成"
					data.BackupAttention = false
				}
			}
		}
	}
	if h.opts.RestoreStatus != nil {
		status, err := h.opts.RestoreStatus.Read()
		if err != nil {
			data.RestoreStatus = "无有效回执"
			data.RestoreDetail = "回执缺失、不可读或格式无效；请检查隔离恢复任务、回传路径与服务日志"
			data.RestoreScope = "无法确认归档曾完整导入、逐表检查并清理临时库；人工语义抽查也尚未证明"
			h.logger.Warn("系统页读取恢复状态回执失败", "error", err)
		} else if !status.TechnicalRestoreVerified || status.SemanticInspection != "not_proven" || status.MaxAge <= 0 {
			data.RestoreStatus = "回执语义无效"
			data.RestoreDetail = "回执没有同时证明隔离导入、逐表 QUICK、行数/迁移枚举与临时库清理"
			data.RestoreScope = "不能把不完整或越界声称人工内容已验证的回执展示为健康"
			h.logger.Warn("系统页拒绝语义无效的恢复状态回执")
		} else {
			now := h.opts.Now()
			age := now.Sub(status.RestoredAt)
			if age < -5*time.Minute {
				data.RestoreStatus = "回执时间异常"
				data.RestoreDetail = "恢复时间显著晚于当前实例时间，请检查隔离环境与应用主机 UTC 时钟"
				data.RestoreScope = "时间异常时不能判断演练是否仍满足恢复策略；人工语义抽查尚未证明"
			} else {
				if age < 0 {
					age = 0
				}
				data.RestoreDetail = fmt.Sprintf(
					"%s · UTC %s · 年龄 %s / 上限 %s · %d 张表 · %d 条迁移（最新 %s）",
					status.Archive, status.RestoredAt.UTC().Format("2006-01-02 15:04:05"),
					displayUptime(age), displayDuration(status.MaxAge), status.TableCount,
					status.MigrationCount, status.LatestMigration,
				)
				data.RestoreScope = "技术恢复已验证：完整导入、逐表 QUICK、行数/迁移枚举和临时库清理；代表性 Memory、Card 与 Shared Line 仍需人工抽查"
				if age > status.MaxAge {
					data.RestoreStatus = "恢复演练已过期"
				} else {
					data.RestoreStatus = "技术恢复演练有效"
					data.RestoreAttention = false
				}
			}
		}
	}
	if h.opts.IntegrityStatus != nil {
		status, err := h.opts.IntegrityStatus.Read()
		if err != nil {
			data.IntegrityStatus = "无有效回执"
			data.IntegrityDetail = "回执缺失、不可读或格式无效；请检查完整性 timer、oneshot 与服务日志"
			h.logger.Warn("系统页读取数据完整性状态回执失败", "error", err)
		} else if status.SchemaVersion != h.opts.SchemaVersion {
			data.IntegrityStatus = "回执版本不匹配"
			data.IntegrityDetail = fmt.Sprintf("回执 schema %s，当前二进制 schema %s；必须用当前 release 重新检查", status.SchemaVersion, h.opts.SchemaVersion)
		} else if status.MaxAge <= 0 || status.RulesChecked <= 0 || (status.Verified && status.Violations != 0) || (!status.Verified && status.Violations == 0) {
			data.IntegrityStatus = "回执语义无效"
			data.IntegrityDetail = "回执的检查结果、规则数、违规数或有效期彼此不一致"
			h.logger.Warn("系统页拒绝语义无效的数据完整性状态回执")
		} else {
			age := h.opts.Now().Sub(status.CheckedAt)
			if age < -5*time.Minute {
				data.IntegrityStatus = "回执时间异常"
				data.IntegrityDetail = "检查时间显著晚于当前实例时间，请检查数据库任务与应用主机 UTC 时钟"
			} else {
				if age < 0 {
					age = 0
				}
				data.IntegrityDetail = fmt.Sprintf(
					"UTC %s · 年龄 %s / 上限 %s · %d 条固定规则",
					status.CheckedAt.UTC().Format("2006-01-02 15:04:05"), displayUptime(age), displayDuration(status.MaxAge), status.RulesChecked,
				)
				if status.Violations > 0 {
					data.IntegrityStatus = fmt.Sprintf("发现 %d 次关系违规", status.Violations)
					data.IntegrityDetail += fmt.Sprintf(" · %d 类：%s · 同一行可能命中多条规则", len(status.Issues), integrityIssueSummary(status.Issues))
				} else if age > status.MaxAge {
					data.IntegrityStatus = "完整性检查已过期"
				} else {
					data.IntegrityStatus = "数据关系检查通过"
					data.IntegrityAttention = false
				}
			}
		}
	}
	if h.opts.TenantCapacity != nil {
		capacityCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		snapshot, err := h.opts.TenantCapacity.ReadTenantCapacity(capacityCtx, p.TenantID)
		cancel()
		if err != nil {
			data.CapacityStatus = "快照不可用"
			data.CapacityDetail = "3 秒内未能完成租户级只读统计；核心读写与公开指标不受影响"
			h.logger.Warn("系统页读取租户容量快照失败", "error", err)
		} else {
			data.Capacity = snapshot
			data.CapacityAvailable = true
			data.CapacityAttention = false
			data.CapacityStatus = "精确逻辑快照"
			data.CapacityTotal = displayBytes(snapshot.TotalLogicalBytes())
			data.KnowledgeCapacity = displayBytes(snapshot.KnowledgeLogicalBytes)
			data.ContinuityCapacity = displayBytes(snapshot.ContinuityLogicalBytes)
			data.EmbeddingCapacity = displayBytes(snapshot.EmbeddingLogicalBytes)
			data.ExternalCapacity = displayBytes(snapshot.ExternalLogicalBytes)
			data.AuditCapacity = displayBytes(snapshot.AuditLogicalBytes)
			data.CapacityMeasuredAt = snapshot.MeasuredAt.UTC().Format("2006-01-02 15:04:05 UTC")
			data.CapacityDetail = fmt.Sprintf(
				"%d 名成员 · %d 个 Agent · %d 个空间 · Memory %d · Card %d · Shared Line %d",
				snapshot.Members, snapshot.Agents, snapshot.Spaces, snapshot.Memories, snapshot.Cards, snapshot.ContinuityLines,
			)
		}
	}
	if h.opts.ExternalConfigured || h.opts.ExternalSources != nil {
		if h.opts.ExternalConfigured {
			data.ExternalStatus = "等待首次同步"
			data.ExternalDetail = "Atlas 已配置，但还没有可展示的同步回执"
			data.ExternalAttention = true
		}
		if h.opts.ExternalSources == nil {
			data.ExternalStatus = "状态未装配"
			data.ExternalDetail = "部署启用了 Atlas，但系统页没有状态读取器"
		} else if sources, err := h.opts.ExternalSources(r.Context(), p.TenantID); err != nil {
			data.ExternalStatus = "状态不可用"
			data.ExternalDetail = "无法读取外部源同步回执，请检查数据库与服务日志"
			h.logger.Warn("系统页读取外部源状态失败", "error", err)
		} else if len(sources) > 0 {
			data.ExternalAttention = false
			totalDocuments := 0
			for _, source := range sources {
				view := externalSourceView{Name: source.Name, System: source.System, LastSuccess: "尚未成功"}
				totalDocuments += source.DocumentCount
				if source.LastSucceededAt != nil {
					view.LastSuccess = source.LastSucceededAt.UTC().Format("2006-01-02 15:04:05 UTC")
				}
				if !h.opts.ExternalConfigured {
					view.Status = "仅镜像（不刷新）"
					view.Detail = fmt.Sprintf("%d 份有效镜像；连接关闭，正文仍在来源端", source.DocumentCount)
					view.Attention = true
				} else {
					switch source.LastSyncStatus {
					case external.SyncSucceeded:
						view.Status = "同步正常"
						view.Detail = fmt.Sprintf("%d 份有效镜像；正文仍在来源端", source.DocumentCount)
					case external.SyncRunning:
						view.Status = "同步中"
						view.Detail = "正在枚举来源并生成 body-free 向量"
						if source.SyncLeaseUntil == nil || !source.SyncLeaseUntil.After(h.opts.Now()) {
							view.Status = "同步租约已过期"
							view.Detail = "本轮可能异常中断；下一实例或下一周期可接管"
							view.Attention = true
						}
					case external.SyncFailed:
						view.Status = "最近同步失败"
						view.Detail = source.LastError
						if view.Detail == "" {
							view.Detail = "失败原因未记录，请检查服务日志"
						}
						view.Attention = true
					default:
						view.Status = "尚未同步"
						view.Detail = "等待 worker 完成第一轮完整快照"
						view.Attention = true
					}
				}
				if view.Attention {
					data.ExternalAttention = true
				}
				data.ExternalSources = append(data.ExternalSources, view)
			}
			if !h.opts.ExternalConfigured {
				data.ExternalStatus = "仅镜像"
				data.ExternalDetail = fmt.Sprintf("%d 个来源，%d 份有效镜像；连接关闭，检索继续但不会刷新", len(sources), totalDocuments)
				data.ExternalAttention = true
			} else {
				data.ExternalDetail = fmt.Sprintf("%d 个来源，%d 份有效镜像；本地不保存外部正文", len(sources), totalDocuments)
				if data.ExternalAttention {
					data.ExternalStatus = "需要关注"
				} else {
					data.ExternalStatus = "同步正常"
				}
			}
		}
	}
	h.render(w, "system.html", data)
}

func displayBytes(value int64) string {
	if value < 0 {
		return "未知"
	}
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	for _, label := range units {
		size /= 1024
		if size < 1024 || label == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", size, label)
		}
	}
	return fmt.Sprintf("%d B", value)
}

func displayDuration(value time.Duration) string {
	if value > 0 && value%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d 天", value/(24*time.Hour))
	}
	if value > 0 && value%time.Hour == 0 {
		return fmt.Sprintf("%d 小时", value/time.Hour)
	}
	if value > 0 && value%time.Minute == 0 {
		return fmt.Sprintf("%d 分钟", value/time.Minute)
	}
	return value.String()
}

func displayUptime(value time.Duration) string {
	seconds := int64(value / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%d 秒", seconds)
	}
	days := seconds / (24 * 60 * 60)
	hours := (seconds / (60 * 60)) % 24
	minutes := (seconds / 60) % 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%d 天 %d 小时", days, hours)
	case days > 0:
		return fmt.Sprintf("%d 天", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%d 小时 %d 分钟", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%d 小时", hours)
	default:
		return fmt.Sprintf("%d 分钟", minutes)
	}
}

func displayHTTPLatency(value time.Duration) string {
	if value <= 0 {
		return "0ms"
	}
	if value < time.Second {
		return fmt.Sprintf("%dms", value.Milliseconds())
	}
	return value.Round(10 * time.Millisecond).String()
}

func displayHTTPP95(value time.Duration, overflow bool) string {
	if overflow {
		return ">30s"
	}
	return displayHTTPLatency(value)
}
