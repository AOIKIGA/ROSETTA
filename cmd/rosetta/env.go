package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// deepSeekKey reads only the credential we use. It never evaluates shell code,
// expands variables, or imports unrelated settings into the process environment.
func deepSeekKey(path string) (string, error) {
	if value, exists := os.LookupEnv("DEEPSEEK_API_KEY"); exists {
		return value, nil // An explicitly empty environment value also wins.
	}
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil // .env is optional; the provider reports a missing key.
	}
	if err != nil {
		return "", fmt.Errorf("cannot open env file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return "", fmt.Errorf("cannot read env file (maximum 64 KiB)")
	}
	return parseDeepSeekKey(string(data))
}

func parseDeepSeekKey(data string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), (64<<10)+1)
	key := ""
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, value, found := strings.Cut(line, "=")
		if strings.TrimSpace(name) != "DEEPSEEK_API_KEY" {
			continue
		}
		invalid := func() (string, error) {
			return "", fmt.Errorf("invalid DEEPSEEK_API_KEY assignment in env file at line %d", lineNumber)
		}
		if !found {
			return invalid()
		}
		value = strings.TrimSpace(value)
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			end := strings.IndexByte(value[1:], value[0])
			if end < 0 {
				return invalid()
			}
			end++
			tail := strings.TrimSpace(value[end+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return invalid()
			}
			value = value[1:end]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return invalid()
		}
		key = value // Last assignment wins inside the file.
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("cannot parse env file")
	}
	return key, nil
}
