package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestDeepSeekRequiresKeyBeforeStarting(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	err := run(context.Background(), []string{"serve", "--provider", "deepseek"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("err=%v", err)
	}
}
