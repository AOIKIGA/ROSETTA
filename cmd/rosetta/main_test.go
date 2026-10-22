package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestDeepSeekRequiresKeyBeforeStarting(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	dir, err := os.MkdirTemp("/tmp", "rs-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	err = run(context.Background(), []string{"serve", "--state-dir", dir, "--provider", "deepseek"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("err=%v", err)
	}
}
