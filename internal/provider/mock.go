// Package provider contains model adapters and an offline mock for learning.
package provider

import (
	"context"
	"time"
)

type Mock struct {
	Delay time.Duration
}

func (Mock) Name() string { return "mock" }

func (m Mock) Stream(ctx context.Context, message string, emit func(string) error) error {
	text := []rune("[模拟模型：未调用 AI] 已收到：" + message)
	for len(text) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.Delay > 0 {
			timer := time.NewTimer(m.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		n := min(4, len(text))
		if err := emit(string(text[:n])); err != nil {
			return err
		}
		text = text[n:]
	}
	return nil
}
