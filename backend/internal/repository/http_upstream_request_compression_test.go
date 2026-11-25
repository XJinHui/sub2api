package repository

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func compressionTestService(enabled bool, minBytes int64, exclude []string) *httpUpstreamService {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamRequestCompression = enabled
	cfg.Gateway.UpstreamRequestCompressionMinBytes = minBytes
	cfg.Gateway.UpstreamRequestCompressionExcludeHosts = exclude
	return &httpUpstreamService{cfg: cfg}
}

func compressionTestBody(size int) []byte {
	prefix := []byte(`{"input":"`)
	suffix := []byte(`"}`)
	if size <= len(prefix)+len(suffix) {
		size = len(prefix) + len(suffix) + 1
	}
	body := make([]byte, 0, size)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte("a"), size-len(prefix)-len(suffix))...)
	body = append(body, suffix...)
	return body
}

func newCompressionTestRequest(t *testing.T, rawURL string, body []byte, contentType string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func readGzipBody(t *testing.T, body io.Reader) []byte {
	t.Helper()
	reader, err := gzip.NewReader(body)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	return decoded
}

func TestMaybeCompressUpstreamRequest_LargeJSON(t *testing.T) {
	body := compressionTestBody(300 * 1024)
	req := newCompressionTestRequest(t, "https://relay.example/v1/responses", body, "application/json")
	svc := compressionTestService(true, 256*1024, nil)

	state := svc.maybeCompressUpstreamRequest(req, 1)
	require.True(t, state.compressed)
	require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	require.Less(t, req.ContentLength, int64(len(body)))

	sent, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, readGzipBody(t, bytes.NewReader(sent)))

	replay, err := req.GetBody()
	require.NoError(t, err)
	replayBody, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, body, readGzipBody(t, bytes.NewReader(replayBody)))

	state.restore()
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.Equal(t, int64(len(body)), req.ContentLength)
	restored, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, restored)
}

func TestMaybeCompressUpstreamRequest_Skips(t *testing.T) {
	large := compressionTestBody(300 * 1024)
	tests := []struct {
		name        string
		body        []byte
		contentType string
		url         string
		mutate      func(*http.Request)
		exclude     []string
	}{
		{
			name:        "below threshold",
			body:        compressionTestBody(100 * 1024),
			contentType: "application/json",
			url:         "https://relay.example/v1/responses",
		},
		{
			name:        "non json",
			body:        large,
			contentType: "text/plain",
			url:         "https://relay.example/v1/responses",
		},
		{
			name:        "default excluded host",
			body:        large,
			contentType: "application/json",
			url:         "https://api.openai.com/v1/responses",
		},
		{
			name:        "custom excluded suffix",
			body:        large,
			contentType: "application/json",
			url:         "https://relay.example/v1/responses",
			exclude:     []string{"*.example"},
		},
		{
			name:        "docker service host",
			body:        large,
			contentType: "application/json",
			url:         "http://workbuddy2api:7863/v1/chat/completions",
		},
		{
			name:        "private ip host",
			body:        large,
			contentType: "application/json",
			url:         "http://10.1.2.3:8080/v1/responses",
		},
		{
			name:        "already encoded",
			body:        large,
			contentType: "application/json",
			url:         "https://relay.example/v1/responses",
			mutate: func(req *http.Request) {
				req.Header.Set("Content-Encoding", "br")
			},
		},
		{
			name:        "not replayable",
			body:        large,
			contentType: "application/json",
			url:         "https://relay.example/v1/responses",
			mutate: func(req *http.Request) {
				req.GetBody = nil
			},
		},
		{
			name:        "digest present",
			body:        large,
			contentType: "application/json",
			url:         "https://relay.example/v1/responses",
			mutate: func(req *http.Request) {
				req.Header.Set("Digest", "sha-256=:abc:")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newCompressionTestRequest(t, tt.url, tt.body, tt.contentType)
			if tt.mutate != nil {
				tt.mutate(req)
			}
			svc := compressionTestService(true, 256*1024, tt.exclude)
			state := svc.maybeCompressUpstreamRequest(req, 1)
			require.False(t, state.compressed)
			require.Empty(t, req.Header.Get("Content-Encoding"))
		})
	}
}

func TestMaybeCompressUpstreamRequest_Disabled(t *testing.T) {
	body := compressionTestBody(300 * 1024)
	req := newCompressionTestRequest(t, "https://relay.example/v1/responses", body, "application/json")
	svc := compressionTestService(false, 256*1024, nil)

	state := svc.maybeCompressUpstreamRequest(req, 1)
	require.False(t, state.compressed)
	require.Empty(t, req.Header.Get("Content-Encoding"))
}

func TestMaybeCompressUpstreamRequest_ZeroMinUsesDefault(t *testing.T) {
	body := compressionTestBody(int(config.DefaultUpstreamRequestCompressionMinBytes) + 1024)
	req := newCompressionTestRequest(t, "https://relay.example/v1/responses", body, "application/json")
	svc := compressionTestService(true, 0, nil)

	state := svc.maybeCompressUpstreamRequest(req, 1)
	require.True(t, state.compressed)
}

func TestDoWithRequestCompression_FallsBackOnUnsupportedEncoding(t *testing.T) {
	original := compressionTestBody(300 * 1024)
	var mu sync.Mutex
	var requests int
	var encodings []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		encoding := r.Header.Get("Content-Encoding")
		mu.Lock()
		requests++
		encodings = append(encodings, encoding)
		mu.Unlock()

		if encoding == "gzip" {
			reader, gzipErr := gzip.NewReader(bytes.NewReader(body))
			if gzipErr != nil {
				t.Errorf("gzip.NewReader: %v", gzipErr)
				return
			}
			decoded, readErr := io.ReadAll(reader)
			_ = reader.Close()
			if readErr != nil {
				t.Errorf("read gzip body: %v", readErr)
				return
			}
			if !bytes.Equal(decoded, original) {
				t.Errorf("compressed request body mismatch: got %d bytes, want %d", len(decoded), len(original))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported content-encoding gzip"}}`))
			return
		}
		if !bytes.Equal(body, original) {
			t.Errorf("plain request body mismatch: got %d bytes, want %d", len(body), len(original))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}}

	req := newCompressionTestRequest(t, "http://relay.example/v1/responses", original, "application/json")
	svc := compressionTestService(true, 256*1024, nil)
	state := svc.maybeCompressUpstreamRequest(req, 1)
	require.True(t, state.compressed)

	resp, err := svc.doWithRequestCompression(client, req, state)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	mu.Lock()
	require.Equal(t, 2, requests)
	require.Equal(t, []string{"gzip", ""}, encodings)
	mu.Unlock()
	require.True(t, svc.isRequestCompressionUnsupported("relay.example"))

	// Once marked unsupported, the next request to the same host is sent plain.
	next := newCompressionTestRequest(t, "http://relay.example/v1/responses", original, "application/json")
	nextState := svc.maybeCompressUpstreamRequest(next, 1)
	require.False(t, nextState.compressed)
}

func TestDoWithRequestCompression_FallsBackOn415(t *testing.T) {
	original := compressionTestBody(300 * 1024)
	var mu sync.Mutex
	var requests int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		mu.Lock()
		requests++
		count := requests
		mu.Unlock()
		if count == 1 {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}}

	req := newCompressionTestRequest(t, "http://relay.example/v1/responses", original, "application/json")
	svc := compressionTestService(true, 256*1024, nil)
	state := svc.maybeCompressUpstreamRequest(req, 1)
	require.True(t, state.compressed)

	resp, err := svc.doWithRequestCompression(client, req, state)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	mu.Lock()
	require.Equal(t, 2, requests)
	mu.Unlock()
}

func TestIsLocalUpstreamHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "workbuddy2api", "svc.local"} {
		require.True(t, isLocalUpstreamHost(host), host)
	}
	for _, host := range []string{"relay.example", "sub2.xiaopozhan.buzz", "8.8.8.8"} {
		require.False(t, isLocalUpstreamHost(host), host)
	}
}

func TestIsExcludedRequestCompressionHost(t *testing.T) {
	patterns := []string{"api.openai.com", "*.googleapis.com"}
	require.True(t, isExcludedRequestCompressionHost("api.openai.com", patterns))
	require.True(t, isExcludedRequestCompressionHost("foo.api.openai.com", patterns))
	require.True(t, isExcludedRequestCompressionHost("generativelanguage.googleapis.com", patterns))
	require.False(t, isExcludedRequestCompressionHost("relay.example", patterns))
}
