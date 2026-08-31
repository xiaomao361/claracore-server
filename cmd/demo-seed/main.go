package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"claracore-server/internal/adapters/crypto"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/platform"
)

func main() {
	mode := flag.String("mode", "inspect", "inspect | apply | cleanup")
	confirm := flag.String("confirm", "", "apply/cleanup 时必须等于固定 seed key")
	tenant := flag.String("tenant", "", "目标租户 ULID；必须显式指定")
	credentialsFile := flag.String("credentials-file", "", "apply 时保存演示账号与 token 的绝对路径")
	flag.Parse()
	if *tenant == "" {
		fatalIf(fmt.Errorf("demo seed: 必须显式给出 -tenant"))
	}

	cfg, err := config.Load()
	fatalIf(err)
	if cfg.MySQL.TLS.Enabled() {
		fatalIf(fmt.Errorf("demo seed: 当前独立工具暂不支持 verify-full MySQL"))
	}
	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: time.Minute})
	fatalIf(err)
	defer db.Close()

	switch *mode {
	case "inspect":
		result, err := mysql.InspectDemoSeed(context.Background(), db, platform.ID(*tenant))
		fatalIf(err)
		writeJSON(result)
	case "apply":
		if *confirm != mysql.DemoSeedKey {
			fatalIf(fmt.Errorf("demo seed: apply 需要 -confirm %s", mysql.DemoSeedKey))
		}
		if !filepath.IsAbs(*credentialsFile) {
			fatalIf(fmt.Errorf("demo seed: credentials-file 必须是绝对路径"))
		}
		credentials, err := mysql.NewDemoCredentials()
		fatalIf(err)
		pending := *credentialsFile + ".pending"
		fatalIf(writeSecretFile(pending, credentials))
		hasher := crypto.NewArgon2id(crypto.Argon2idParams{Memory: uint32(cfg.Password.MemoryKiB), Iterations: uint32(cfg.Password.Iterations), Parallelism: uint8(cfg.Password.Parallelism), SaltLength: 16, KeyLength: 32})
		result, err := mysql.ApplyDemoSeed(context.Background(), db, hasher, platform.ID(*tenant), credentials, time.Now())
		if err != nil {
			_ = os.Remove(pending)
			fatalIf(err)
		}
		if result.AlreadyApplied {
			_ = os.Remove(pending)
		} else {
			fatalIf(os.Rename(pending, *credentialsFile))
		}
		writeJSON(result)
	case "cleanup":
		if *confirm != mysql.DemoSeedKey {
			fatalIf(fmt.Errorf("demo seed: cleanup 需要 -confirm %s", mysql.DemoSeedKey))
		}
		manifest, err := mysql.CleanupDemoSeed(context.Background(), db, platform.ID(*tenant))
		fatalIf(err)
		writeJSON(map[string]any{"cleaned": true, "manifest": manifest})
	default:
		fatalIf(fmt.Errorf("demo seed: 未知 mode %q", *mode))
	}
}

func writeSecretFile(path string, credentials []mysql.DemoCredential) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := json.NewEncoder(file).Encode(map[string]any{"seedKey": mysql.DemoSeedKey, "credentials": credentials}); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func writeJSON(value any) {
	fatalIf(json.NewEncoder(os.Stdout).Encode(value))
}

func fatalIf(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
