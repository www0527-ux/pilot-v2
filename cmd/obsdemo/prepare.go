package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"

	"shanyangcode.com/pilot/internal/config"
)

// Preserve the original YAML's env references instead of writing expanded keys.
func isolatedConfig(raw []byte, dimension int) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	section := func(key string) map[string]any {
		v, ok := document[key].(map[string]any)
		if !ok {
			v = map[string]any{}
			document[key] = v
		}
		return v
	}
	server := section("server")
	server["address"], server["base_url"], server["env"] = "127.0.0.1:18080", "http://localhost:18080", "demo"
	section("database")["name"] = "pilot_demo"
	section("redis")["db"] = 1
	section("elasticsearch")["knowledge_index"] = fmt.Sprintf("pilot_demo_knowledge_%d", dimension)
	ob := section("observability")
	ob["service_name"] = "pilot-demo-console"
	addresses, ok := section("elasticsearch")["addresses"].([]any)
	if !ok || len(addresses) == 0 {
		return nil, errors.New("source config must explicitly configure elasticsearch.addresses")
	}
	ob["log"] = map[string]any{"url": addresses[0], "index": demoIndex, "slow_request_ms": 1000}
	traceConfig, ok := ob["trace"].(map[string]any)
	if !ok {
		traceConfig = map[string]any{}
		ob["trace"] = traceConfig
	}
	traceConfig["sample_ratio"] = 1
	section("memory")["episodic"] = map[string]any{"mode": "sql", "index": "pilot_demo_episodic", "top_k": 3}
	section("mcp")["enabled"] = false
	return yaml.Marshal(document)
}

func prepare(configPath, output string, cfg *config.Config) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	isolated, err := isolatedConfig(raw, cfg.Embedding.Dimension)
	if err != nil {
		return err
	}
	if _, err := config.Parse(isolated); err != nil {
		return err
	}
	dbCfg := mysql.NewConfig()
	dbCfg.Net = "tcp"
	dbCfg.Addr = net.JoinHostPort(cfg.Database.Host, strconv.Itoa(cfg.Database.Port))
	dbCfg.User, dbCfg.Passwd = cfg.Database.User, cfg.Database.Password
	connector, err := mysql.NewConnector(dbCfg)
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// A fixed dedicated database: never drop, truncate, or alter the source DB.
	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS pilot_demo CHARACTER SET utf8mb4"); err != nil {
		return fmt.Errorf("create dedicated demo database: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(output, isolated, 0600); err != nil {
		return err
	}
	fmt.Println("Prepared isolated demo config and pilot_demo database; source credentials remain local.")
	return nil
}
