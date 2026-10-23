package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
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
	for _, s := range []int{403, 410, 500, 502, 503} {
		status = s
		if _, err := c.Ingest(context.Background(), nil); Classify(err) != Retry {
			t.Fatalf("%d should retry", s)
		}
	}
	status = 401
	if _, err := c.Ingest(context.Background(), nil); Classify(err) != Unauthorized {
		t.Fatal("401 should be classified as unauthorized")
	}
	srv.Close()
	if _, err := c.Ingest(context.Background(), nil); Classify(err) != Retry {
		t.Fatal("network errors should retry")
	}
}

func TestHostDeletedIsItsOwnDisposition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"HOST_DELETED","message":"This host was deleted."}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "hst_t").Ingest(context.Background(), nil)
	if Classify(err) != HostDeleted {
		t.Fatalf("410 HOST_DELETED: %v -> %v", err, Classify(err))
	}
}

// Every request carries the agent's clock at send time, and the server's
// clock offset and update hint are parsed from responses.
func TestAgentTimeHeaderAndClockOffset(t *testing.T) {
	var mu sync.Mutex
	var stamps []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms, err := strconv.ParseInt(r.Header.Get(AgentTimeHeader), 10, 64)
		if err != nil {
			t.Errorf("%s: bad %s header %q", r.URL.Path, AgentTimeHeader, r.Header.Get(AgentTimeHeader))
		}
		mu.Lock()
		stamps = append(stamps, ms)
		mu.Unlock()
		if r.URL.Path == "/agent-api/v2/register" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"host_uuid":"u","host_token":"hst_x","clock_offset_ms":-42000}`))
			return
		}
		_, _ = w.Write([]byte(`{"accepted_samples":1,"clock_offset_ms":31000,` +
			`"agent_update":{"latest_version":"2.1.0","release_url":"https://github.com/spectate/agent/releases/tag/v2.1.0"}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "hst_t")

	before := time.Now().UnixMilli()
	reg, err := c.Register(context.Background(), RegisterRequest{Token: "t"})
	if err != nil || reg.ClockOffsetMs != -42000 {
		t.Fatalf("register = %+v, %v", reg, err)
	}
	time.Sleep(20 * time.Millisecond) // a second request is stamped at its own send time
	resp, err := c.Ingest(context.Background(), nil)
	if err != nil || resp.ClockOffsetMs != 31000 || resp.AgentUpdate == nil || resp.AgentUpdate.LatestVersion != "2.1.0" {
		t.Fatalf("ingest = %+v, %v", resp, err)
	}
	after := time.Now().UnixMilli()

	mu.Lock()
	defer mu.Unlock()
	if len(stamps) != 2 {
		t.Fatalf("stamps = %v", stamps)
	}
	for _, ms := range stamps {
		if ms < before || ms > after {
			t.Fatalf("%s = %d, want within [%d, %d]", AgentTimeHeader, ms, before, after)
		}
	}
	if stamps[1]-stamps[0] < 20 {
		t.Fatalf("header must be set per request at send time: %v", stamps)
	}

	// Absent fields stay zero.
	var plain IngestResponse
	if err := json.Unmarshal([]byte(`{"accepted_samples":1,"agent_update":null}`), &plain); err != nil || plain.AgentUpdate != nil || plain.ClockOffsetMs != 0 {
		t.Fatalf("plain = %+v, %v", plain, err)
	}
}
