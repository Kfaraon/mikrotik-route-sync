package collectors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker"
)

type HTTP struct {
	client  *retryablehttp.Client
	breaker *gobreaker.CircuitBreaker
	limit   int64
}

func NewHTTP(timeout time.Duration, cfg config.RetryConfig, maxMB int) *HTTP {
	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient.Timeout = timeout
	retryClient.RetryMax = cfg.MaxAttempts
	if cfg.BaseDelay.Duration() > 0 {
		retryClient.RetryWaitMin = cfg.BaseDelay.Duration()
	}
	if cfg.MaxDelay.Duration() > 0 {
		retryClient.RetryWaitMax = cfg.MaxDelay.Duration()
	}

	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "external",
		MaxRequests: 3,
		Interval:    60 * time.Second,
		Timeout:     60 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 5 && failureRatio >= 0.6
		},
	})

	return &HTTP{
		client:  retryClient,
		breaker: cb,
		limit:   int64(maxMB * 1024 * 1024),
	}
}

func (h *HTTP) Fetch(ctx context.Context, url string) ([]byte, error) {
	var out []byte
	err := h.breaker.Execute(func() (interface{}, error) {
		req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "mikrotik-route-sync/1.0")

		resp, err := h.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
		}

		limited := io.LimitReader(resp.Body, h.limit)
		b, err := io.ReadAll(limited)
		if err != nil {
			return nil, err
		}
		out = b
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
