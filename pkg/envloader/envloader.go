package envloader

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const envFileName = ".env"

// Load 从当前目录向上查找 .env，并将其中的键值注入进程环境变量。
// 若环境变量已存在，则保持现有值不覆盖，便于外部注入优先。
func Load() error {
	envPath, err := findEnvFile()
	if err != nil {
		return err
	}
	if envPath == "" {
		return nil
	}

	file, err := os.Open(envPath)
	if err != nil {
		return fmt.Errorf("open %s failed: %w", envPath, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid .env line %d: %s", lineNo, line)
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			return fmt.Errorf("invalid .env line %d: empty key", lineNo)
		}

		value = trimQuotes(value)

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set env %s failed: %w", key, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s failed: %w", envPath, err)
	}
	return nil
}

func findEnvFile() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory failed: %w", err)
	}

	for {
		candidate := filepath.Join(dir, envFileName)
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() {
			return candidate, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("stat %s failed: %w", candidate, statErr)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", nil
}

func trimQuotes(value string) string {
	if len(value) < 2 {
		return value
	}
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		return value[1 : len(value)-1]
	}
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		return value[1 : len(value)-1]
	}
	return value
}
