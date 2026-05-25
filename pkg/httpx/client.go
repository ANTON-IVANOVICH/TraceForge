package httpx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	http    *http.Client
	retries int
	backoff time.Duration
}

func NewClient(timeout time.Duration, retries int, backoff time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if retries < 0 {
		retries = 0
	}
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}

	return &Client{
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		retries: retries,
		backoff: backoff,
	}
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := resetBody(req); err != nil {
				return nil, err
			}
		}

		resp, err := c.http.Do(req)
		if !shouldRetry(resp, err) || attempt >= c.retries {
			if err != nil {
				return nil, err
			}
			return resp, nil
		}

		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		wait := c.backoff * time.Duration(1<<attempt)
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}

func resetBody(req *http.Request) error {
	if req.GetBody == nil {
		if req.Body == nil || req.Body == http.NoBody {
			return nil
		}
		return errors.New("request body is not replayable for retry")
	}

	body, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("reset request body: %w", err)
	}
	req.Body = body
	return nil
}

func shouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) {
			return true
		}
		return true
	}
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return true
	}
	return false
}
