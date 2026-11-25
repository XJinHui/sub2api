package repository

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	// upstreamRequestCompressionUnsupportedTTL bounds how long a host is remembered
	// as not accepting gzip request bodies. After it expires we probe again, so an
	// upstream that later adds support regains compression without a restart.
	upstreamRequestCompressionUnsupportedTTL = time.Hour

	// upstreamRequestCompressionMaxBytes protects memory on very large bodies.
	// The request body is already buffered by the gateway; compressing adds another
	// copy. Long-context Codex turns are ~14MB, far below this ceiling.
	upstreamRequestCompressionMaxBytes = 64 << 20

	// upstreamRequestCompressionProbeLimit bounds how much of an error response we
	// read when deciding whether a compressed request was rejected because the
	// upstream does not understand Content-Encoding: gzip.
	upstreamRequestCompressionProbeLimit = 16 << 10
)

// upstreamRequestCompressionState describes how one request was compressed.
// restore puts the request back into its original uncompressed shape so a host
// that rejects gzip can be retried once without it.
type upstreamRequestCompressionState struct {
	host       string
	compressed bool
	restore    func()
}

// maybeCompressUpstreamRequest gzip-compresses a JSON request body at or above
// the configured threshold. It is deliberately conservative:
//
//   - only application/json and application/*+json bodies;
//   - only replayable bodies (GetBody != nil) with a known Content-Length;
//   - never bodies that already carry Content-Encoding or a digest;
//   - never local/private upstreams (they are not bandwidth constrained and
//     most local containers do not decompress request bodies);
//   - never hosts on the exclusion list.
//
// The upstream HTTP service is the single chokepoint for every gateway protocol,
// so this covers OpenAI/Responses, Anthropic and Gemini forwards alike.
func (s *httpUpstreamService) maybeCompressUpstreamRequest(req *http.Request, accountID int64) upstreamRequestCompressionState {
	state := upstreamRequestCompressionState{}
	if s == nil || s.cfg == nil || req == nil || req.URL == nil {
		return state
	}
	gateway := s.cfg.Gateway
	if !gateway.UpstreamRequestCompression {
		return state
	}
	if req.Body == nil || req.Body == http.NoBody || req.GetBody == nil {
		return state
	}
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return state
	}
	if req.ContentLength < 0 || req.ContentLength > upstreamRequestCompressionMaxBytes {
		return state
	}
	minBytes := gateway.UpstreamRequestCompressionMinBytes
	if minBytes <= 0 {
		minBytes = config.DefaultUpstreamRequestCompressionMinBytes
	}
	if req.ContentLength < minBytes {
		return state
	}
	if req.Header.Get("Content-Encoding") != "" {
		return state
	}
	if req.Header.Get("Content-MD5") != "" ||
		req.Header.Get("Digest") != "" ||
		req.Header.Get("Content-Digest") != "" {
		return state
	}
	if !isCompressibleUpstreamContentType(req.Header.Get("Content-Type")) {
		return state
	}
	host := strings.ToLower(strings.TrimSpace(req.URL.Hostname()))
	if host == "" ||
		isLocalUpstreamHost(host) ||
		s.isRequestCompressionUnsupported(host) ||
		isExcludedRequestCompressionHost(host, gateway.UpstreamRequestCompressionExcludeHosts) {
		return state
	}

	originalReader, err := req.GetBody()
	if err != nil {
		return state
	}
	originalBody, err := io.ReadAll(io.LimitReader(originalReader, upstreamRequestCompressionMaxBytes+1))
	_ = originalReader.Close()
	if err != nil || int64(len(originalBody)) < minBytes || len(originalBody) > upstreamRequestCompressionMaxBytes {
		return state
	}

	compressedBody, err := gzipUpstreamRequestBody(originalBody)
	if err != nil || len(compressedBody) == 0 || len(compressedBody) >= len(originalBody) {
		return state
	}

	restore := func() {
		req.Body = io.NopCloser(bytes.NewReader(originalBody))
		req.ContentLength = int64(len(originalBody))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(originalBody)), nil
		}
		req.Header.Del("Content-Encoding")
	}

	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(compressedBody))
	req.ContentLength = int64(len(compressedBody))
	req.TransferEncoding = nil
	req.Header.Set("Content-Encoding", "gzip")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressedBody)), nil
	}

	state.host = host
	state.compressed = true
	state.restore = restore
	slog.Debug("upstream_request_compressed",
		"host", host,
		"account_id", accountID,
		"original_bytes", len(originalBody),
		"compressed_bytes", len(compressedBody),
	)
	return state
}

// doWithRequestCompression sends one request and, if a compressed body was
// rejected with an encoding-related 400/415, retries the same request once
// uncompressed and remembers the host for a while. This keeps the global switch
// safe for upstreams that do not implement request decompression.
func (s *httpUpstreamService) doWithRequestCompression(client *http.Client, req *http.Request, state upstreamRequestCompressionState) (*http.Response, error) {
	resp, err := doUpstreamRequest(client, req, s.upstreamDrainTimeout())
	if err != nil || !state.compressed {
		return resp, err
	}
	if !s.shouldRetryUpstreamRequestUncompressed(resp) {
		return resp, nil
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	s.markRequestCompressionUnsupported(state.host, resp.StatusCode)
	if state.restore != nil {
		state.restore()
	}
	return doUpstreamRequest(client, req, s.upstreamDrainTimeout())
}

func (s *httpUpstreamService) shouldRetryUpstreamRequestUncompressed(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusUnsupportedMediaType:
		return true
	case http.StatusBadRequest:
	default:
		return false
	}

	body, ok := bufferSmallResponseBody(resp, upstreamRequestCompressionProbeLimit)
	if !ok {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"content-encoding",
		"gzip",
		"decompress",
		"unsupported media",
		"invalid request body",
		"invalid json",
		"invalid character",
		"unexpected end of json",
		"请求被拒绝",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (s *httpUpstreamService) isRequestCompressionUnsupported(host string) bool {
	raw, ok := s.requestCompressionUnsupported.Load(host)
	if !ok {
		return false
	}
	expireAt, ok := raw.(time.Time)
	if !ok {
		s.requestCompressionUnsupported.Delete(host)
		return false
	}
	if time.Now().Before(expireAt) {
		return true
	}
	s.requestCompressionUnsupported.Delete(host)
	return false
}

func (s *httpUpstreamService) markRequestCompressionUnsupported(host string, statusCode int) {
	if host == "" {
		return
	}
	s.requestCompressionUnsupported.Store(host, time.Now().Add(upstreamRequestCompressionUnsupportedTTL))
	slog.Warn("upstream_request_compression_unsupported",
		"host", host,
		"status_code", statusCode,
		"retry_without_compression", true,
	)
}

func isCompressibleUpstreamContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// isLocalUpstreamHost skips loopback/private targets and bare Docker service
// names. Local containers are not limited by public egress bandwidth, and many
// of them (plain Gin/Express apps) do not decompress request bodies.
func isLocalUpstreamHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" || host == "localhost.localdomain" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() ||
			ip.IsPrivate() ||
			ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified()
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".localhost", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func isExcludedRequestCompressionHost(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, raw := range patterns {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		pattern = strings.TrimSuffix(pattern, ".")
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			if strings.HasSuffix(host, strings.TrimPrefix(pattern, "*")) {
				return true
			}
			continue
		}
		if host == pattern || strings.HasSuffix(host, "."+pattern) {
			return true
		}
	}
	return false
}

func gzipUpstreamRequestBody(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(body)/4 + 64)
	writer, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(body); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
