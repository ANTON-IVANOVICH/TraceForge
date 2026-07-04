package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"metrics-system/internal/model"
	"metrics-system/pkg/httpx"
)

type Sender struct {
	endpoint string
	client   *httpx.Client
}

func NewSender(endpoint string, client *httpx.Client) *Sender {
	if client == nil {
		client = httpx.NewClient(10*time.Second, 2, 200*time.Millisecond)
	}
	return &Sender{
		endpoint: strings.TrimSpace(endpoint),
		client:   client,
	}
}

func (s *Sender) Send(ctx context.Context, batch model.Batch) error {
	if err := batch.Validate(); err != nil {
		return fmt.Errorf("invalid batch: %w", err)
	}

	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", batch.AgentID)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if len(payload) == 0 {
			return fmt.Errorf("server returned %d", resp.StatusCode)
		}
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	return nil
}
