// Package transport talks to the Spectate v2 agent API.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/version"
)

const (
	registerPath = "/agent-api/v2/register"
	ingestPath   = "/agent-api/v2/ingest"
	maxResponse  = 1 << 20
)

// Client is an HTTP client for one API base URL.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client with conservative timeouts.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        2,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// Error is a non-2xx API response.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("api: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("api: HTTP %d", e.Status)
}

// Disposition says what to do with a batch after a failed send.
type Disposition int

const (
	// Retry later (network error, 5xx, 408, 429, auth problems).
	Retry Disposition = iota
	// Drop: the server will never accept this batch (400, 413, 422).
	Drop
)

// Classify maps an ingest error to a disposition.
func Classify(err error) Disposition {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return Drop
		}
	}
	return Retry
}

// RetryAfter returns the server-requested delay, if any.
func RetryAfter(err error) time.Duration {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

// RegisterRequest is the JSON body of POST /agent-api/v2/register.
type RegisterRequest struct {
	Token        string              `json:"token"`
	Hostname     string              `json:"hostname,omitempty"`
	HostInfo     map[string]string   `json:"host_info,omitempty"`
	CPUInfo      []map[string]string `json:"cpu_info,omitempty"`
	AgentVersion string              `json:"agent_version"`
	HostInfoHash string              `json:"host_info_hash,omitempty"`
}

// RegisterResponse is returned by register.
type RegisterResponse struct {
	HostUUID     string        `json:"host_uuid"`
	HostToken    string        `json:"host_token"`
	Config       config.Remote `json:"config"`
	NextIngestIn int           `json:"next_ingest_in"`
}

// IngestResponse is returned by ingest.
type IngestResponse struct {
	AcceptedSamples int           `json:"accepted_samples"`
	Duplicate       bool          `json:"duplicate"`
	ResendHostInfo  bool          `json:"resend_host_info"`
	Config          config.Remote `json:"config"`
	NextIngestIn    int           `json:"next_ingest_in"`
}

// Register exchanges an install token for a host token.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+registerPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var out RegisterResponse
	if err := c.do(httpReq, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ingest ships one gzip-compressed v2 batch.
func (c *Client) Ingest(ctx context.Context, gzBody []byte) (*IngestResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+ingestPath, bytes.NewReader(gzBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf; proto=spectate.agent.v2.IngestRequest")
	httpReq.Header.Set("Content-Encoding", "gzip")
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)

	var out IngestResponse
	if err := c.do(httpReq, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &Error{Status: resp.StatusCode}
		var payload struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &payload) == nil {
			apiErr.Code = payload.Error.Code
			apiErr.Message = payload.Error.Message
			if apiErr.Message == "" {
				apiErr.Message = payload.Message
			}
		}
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
				apiErr.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return apiErr
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
