package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegisterSendsJSONAndParsesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RegisterRequest
		if r.URL.Path != "/agent-api/v2/register" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Token != "install" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"INVALID_INSTALL_TOKEN","message":"Invalid or expired install token."}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"host_uuid":"u-1","host_token":"hst_x","next_ingest_in":60,"config":{"collection_interval":15,"ingest_interval":60,"collectors":["cpu"]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "")
	resp, err := c.Register(context.Background(), RegisterRequest{Token: "install", AgentVersion: "2"})
	if err != nil || resp.HostToken != "hst_x" || resp.Config.CollectionInterval != 15 {
		t.Fatalf("register = %+v, %v", resp, err)
	}

	_, err = c.Register(context.Background(), RegisterRequest{Token: "bad"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || apiErr.Code != "INVALID_INSTALL_TOKEN" {
		t.Fatalf("err = %v", err)
	}
}

func TestIngestErrorsAreClassified(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Authorization") != "Bearer hst_t" {
			t.Errorf("missing headers: %v", r.Header)
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := New(srv.URL, "hst_t")

	_, err := c.Ingest(context.Background(), []byte{0x1f, 0x8b})
	if Classify(err) != Retry || RetryAfter(err) != 7*time.Second {
		t.Fatalf("429: %v / %v", Classify(err), RetryAfter(err))
	}
	for _, s := range []int{400, 413, 422} {
		status = s
		if _, err := c.Ingest(context.Background(), nil); Classify(err) != Drop {
			t.Fatalf("%d should drop", s)
		}
	}
	for _, s := range []int{401, 403, 500, 502, 503} {
		status = s
		if _, err := c.Ingest(context.Background(), nil); Classify(err) != Retry {
			t.Fatalf("%d should retry", s)
		}
	}
	srv.Close()
	if _, err := c.Ingest(context.Background(), nil); Classify(err) != Retry {
		t.Fatal("network errors should retry")
	}
}
