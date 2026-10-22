// Package client talks HTTP over a local Unix socket, never a TCP port.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"rosetta/internal/protocol"
)

type Client struct {
	http *http.Client
}

func New(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Health(ctx context.Context) (protocol.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := c.request(ctx, "GET", "/v1/health", nil)
	if err != nil {
		return protocol.Health{}, err
	}
	defer resp.Body.Close()
	var health protocol.Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil {
		return health, err
	}
	if health.Protocol != protocol.Version || health.Status != "ready" {
		return health, fmt.Errorf("daemon is not ready or has an incompatible protocol")
	}
	return health, nil
}

func (c *Client) Chat(ctx context.Context, req protocol.ChatRequest, onEvent func(protocol.Event) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute+5*time.Second)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, "POST", "/v1/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	seq, runID := 0, ""
	for scanner.Scan() {
		var event protocol.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("invalid stream event: %w", err)
		}
		seq++
		if seq == 1 {
			runID = event.RunID
			if event.Type != protocol.Started {
				return fmt.Errorf("stream did not start with turn.started")
			}
		}
		if event.Version != protocol.Version || event.RunID == "" || event.RunID != runID || event.SessionID != req.SessionID || event.Seq != seq {
			return fmt.Errorf("stream identity or sequence mismatch")
		}
		switch event.Type {
		case protocol.Started:
			if seq != 1 {
				return fmt.Errorf("duplicate turn.started")
			}
		case protocol.TextDelta, protocol.Completed, protocol.Failed, protocol.RoundStarted, protocol.ToolStarted, protocol.ToolFinished:
		default:
			return fmt.Errorf("unsupported event type: %s", event.Type)
		}
		if err := onEvent(event); err != nil {
			return err
		}
		if event.Type == protocol.Failed {
			return fmt.Errorf("turn failed: %s", event.Error)
		}
		if event.Type == protocol.Completed {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream disconnected before turn.completed; result is incomplete")
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://rosetta"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon (start 'rosetta serve' first): %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var apiErr protocol.Error
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&apiErr); err != nil {
			return nil, fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
	}
	return resp, nil
}
