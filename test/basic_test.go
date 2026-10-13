package test

import (
	"context"
	"os"
	"testing"
	"time"

	"shanyangcode.com/pilot/internal/config"
	"shanyangcode.com/pilot/internal/memory"
	"shanyangcode.com/pilot/pkg/elasticsearch"
	"shanyangcode.com/pilot/pkg/redis"
	"shanyangcode.com/pilot/pkg/types"
)

var cfg *config.Config

func TestMain(m *testing.M) {
	// Ensure tests can locate config.yaml regardless of the package working directory.
	path := os.Getenv(config.EnvPathKey)
	if path == "" {
		path = "../manifest/config/config.yaml"
	}

	loaded, err := config.Load(path)
	if err != nil {
		panic("load config for integration tests failed: " + err.Error())
	}
	cfg = loaded
	os.Exit(m.Run())
}

// TestRedisConnection 测试 Redis 连接
func TestRedisConnection(t *testing.T) {
	ctx := context.Background()

	// 注意：需要先启动 Docker 服务
	client, err := redis.New(ctx, cfg.Redis)
	if err != nil {
		t.Skipf("Redis not available: %v (run 'make up' first)", err)
		return
	}
	defer client.Close()

	t.Log("✅ Redis connection successful")
}

// TestElasticsearchConnection 测试 Elasticsearch 连接
func TestElasticsearchConnection(t *testing.T) {
	ctx := context.Background()

	if _, err := elasticsearch.New(ctx, cfg.Elasticsearch); err != nil {
		t.Skipf("Elasticsearch not available: %v (run 'make up' first)", err)
		return
	}

	t.Log("✅ Elasticsearch connection successful")
}

// TestDatabaseConnection 测试数据库连接
func TestDatabaseConnection(t *testing.T) {
	ctx := context.Background()

	db, err := memory.OpenDB(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Database not available: %v (run 'make up' first)", err)
		return
	}
	defer memory.CloseDB(db)

	t.Log("✅ Database connection successful")
}

// TestShortTermMemory 测试短期记忆
func TestShortTermMemory(t *testing.T) {
	ctx := context.Background()

	client, err := redis.New(ctx, cfg.Redis)
	if err != nil {
		t.Skip("Redis not available")
		return
	}
	defer client.Close()

	// 创建短期记忆管理器
	memMgr := memory.NewShortTermMemory(client, cfg.Redis.Memory)

	// 测试保存消息
	userID := "test_user_" + time.Now().Format("20060102150405")
	sessionID := "test_session_" + time.Now().Format("150405.000000000")

	msg := types.ChatMessage{
		Role:      "user",
		Content:   "Hello, this is a test message",
		Timestamp: time.Now(),
	}

	if err := memMgr.SaveMessage(ctx, userID, sessionID, msg); err != nil {
		t.Fatalf("Failed to save message: %v", err)
	}
	defer memMgr.ClearMemory(ctx, userID, sessionID)

	// 测试获取消息
	messages, err := memMgr.GetMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("Failed to get messages: %v", err)
	}

	if len(messages) == 0 {
		t.Fatal("No messages retrieved")
	}

	lastMessage := messages[len(messages)-1]
	if lastMessage.Content != msg.Content {
		t.Errorf("Message content mismatch: got %s, want %s", lastMessage.Content, msg.Content)
	}

	t.Log("✅ Short-term memory test passed")
}
