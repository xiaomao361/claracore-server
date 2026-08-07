.PHONY: help build run migrate test itest check fmt vet arch cover clean

BINARY := claracore-server

# domain/ 的覆盖率门槛（docs/05-code-structure.md §7，2026-08-06 定）。
# 只卡 domain —— adapters 的测试要真库，CI 未必有。
COVERAGE_MIN  := 85
COVERAGE_OUT  := .coverage.out

help:
	@echo "make build   编译到 ./$(BINARY)"
	@echo "make run     本地运行（需要 .env）"
	@echo "make migrate 应用数据库迁移（需要 .env）"
	@echo "make test    全部测试"
	@echo "make cover   domain/ 覆盖率门槛（$(COVERAGE_MIN)%）"
	@echo "make check   CI 检查：fmt + vet + 架构约束 + 测试"
	@echo "make clean   清理构建产物"

build:
	go build -o $(BINARY) ./cmd/server

run:
	go run ./cmd/server

# 迁移是显式动作，服务启动时不会隐式改表结构
migrate:
	go run ./cmd/server -migrate

test:
	go test ./... -count=1

# 集成测试：需要真实 MySQL（.env 中配置的库）。覆盖三件事——
#   test/ schema             迁移在库里的实际结果
#   adapters/mysql/          Store 实现与领域层 fake 的行为一致性
#   test/ e2e                完整装配：真库 + 真 token 打通 MCP
# 不进 make check —— CI 未必有库；但改完迁移或 SQL 必须手跑一次。
#
# 刻意不加 -run 过滤：原先按测试名列白名单，结果新增的集成测试因为没被写进
# 名单而静默不跑，全绿骗了一次（2026-08-07）。同包内的普通测试会被一并重跑，
# 那点耗时远比"漏跑了什么自己不知道"划算。
itest:
	go test ./test/ ./internal/adapters/mysql/ -tags=integration -count=1 -v

# 领域层覆盖率门槛。写在文档里的门槛不是门槛——它得能让 CI 红。
# 可临时提高门槛来验证它真的会失败：make cover COVERAGE_MIN=95
cover:
	@go test ./internal/domain/... -coverprofile=$(COVERAGE_OUT) -count=1 > /dev/null
	@go tool cover -func=$(COVERAGE_OUT) | awk -v min=$(COVERAGE_MIN) '/^total:/ { \
		pct = $$NF; gsub(/%/, "", pct); \
		if (pct + 0 < min + 0) { \
			printf("domain/ 覆盖率 %s%%，低于门槛 %s%%\n", pct, min); \
			printf("  低于门槛通常意味着有整块逻辑没被测到，而不是差几行注释\n"); \
			exit 1 } \
		printf("domain/ 覆盖率 %s%%（门槛 %s%%）\n", pct, min) }'

# check 是进 CI 的完整门槛。任何一项失败即失败。
# 架构约束与 schema 规则由 test/ 下的测试保障，见 docs/05-code-structure.md §5
check: fmt vet arch cover test
	@echo "全部检查通过"

fmt:
	@out=$$(gofmt -l . 2>/dev/null | grep -v '^poc/' || true); \
	if [ -n "$$out" ]; then \
		echo "以下文件未格式化，请运行 gofmt -w ："; echo "$$out"; exit 1; \
	fi

vet:
	go vet ./...

# arch 单独可跑，便于在开发中快速验证结构是否被破坏
arch:
	go test ./test/ -run 'TestArchitecture|TestDomain|TestMigrations' -count=1 -v

clean:
	rm -f $(BINARY) $(COVERAGE_OUT)
