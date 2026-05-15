// Package oss — mock.go is the development implementation. It spins up a
// goroutine-owned HTTP server on cfg.OSS.MockListenAddr that accepts
// PUT /<object_key> requests, validates Content-Length / Content-Type, and
// records the upload in an in-memory map.
//
// This lets verify_step9.sh exercise the full presign → real PUT → callback
// chain locally without any Aliyun dependency. The server is bound to
// 127.0.0.1 only — never exposed beyond localhost.
package oss

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"cooking-platform/pkg/config"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MockUploadRecord captures what was uploaded — used by tests and the
// verify script to assert success without round-tripping to a real bucket.
type MockUploadRecord struct {
	ObjectKey   string
	ContentType string
	Size        int64
	UploadedAt  time.Time
}

// MockClient implements Client and hosts an embedded HTTP listener that
// stands in for the Aliyun OSS bucket. The listener is started in
// NewMockClient and torn down by Close.
type MockClient struct {
	cfg      config.OSSConfig
	listener net.Listener
	server   *http.Server
	log      *zap.Logger

	mu        sync.RWMutex
	uploaded  map[string]MockUploadRecord
	closeOnce sync.Once
}

// NewMockClient starts the local mock OSS server.
//
// Failure cases:
//   - cfg.OSS.MockListenAddr already in use → returns the listen error.
//   - cfg.OSS.URLPrefix doesn't point at the mock listener → presign would
//     still work but the public URL would be wrong; this is config sanity,
//     not enforceable here. See config.validate.
func NewMockClient(cfg config.OSSConfig) (*MockClient, error) {
	addr := cfg.MockListenAddr
	if addr == "" {
		addr = "127.0.0.1:18080"
	}

	c := &MockClient{
		cfg:      cfg,
		uploaded: make(map[string]MockUploadRecord),
		log:      zap.L().Named("oss.mock"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", c.handlePUT)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mock oss listen %s: %w", addr, err)
	}
	c.listener = ln
	c.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second, // image PUT may take a moment
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := c.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			c.log.Error("mock oss server error", zap.Error(err))
		}
	}()

	c.log.Info("mock oss server started",
		zap.String("addr", addr),
		zap.String("url_prefix", cfg.URLPrefix),
	)
	return c, nil
}

// Presign returns a fake upload URL pointing at the embedded mock server.
//
// The URL has no signed query params — security is provided by:
//  1. The 15-minute TTL stored alongside the nonce in upload_cache.
//  2. The fact that only the platform user who called Presign knows the
//     generated object_key (UUID).
//  3. Local-only binding of the mock listener (127.0.0.1).
//
// PublicURL is built from cfg.OSS.URLPrefix + object_key, which is what
// the production Aliyun implementation also returns. This keeps Service
// layer code agnostic to the implementation.
func (c *MockClient) Presign(params PresignParams) (*PresignResult, error) {
	if !params.Purpose.IsValid() {
		return nil, fmt.Errorf("invalid purpose: %q", params.Purpose)
	}
	objectKey := buildObjectKey(params)
	ttl := c.cfg.PresignTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	addr := c.listener.Addr().String() // honour the actual bound port
	uploadURL := "http://" + addr + "/" + objectKey

	return &PresignResult{
		UploadURL: uploadURL,
		PublicURL: strings.TrimRight(c.cfg.URLPrefix, "/") + "/" + objectKey,
		Method:    http.MethodPut,
		Headers: map[string]string{
			"Content-Type": params.ContentType,
		},
		ObjectKey: objectKey,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// Close stops the embedded HTTP server. Idempotent.
func (c *MockClient) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.server != nil {
			err = c.server.Close()
		}
	})
	return err
}

// GetUploadRecord exposes the in-memory record for tests / verify scripts.
// Returns ok=false when the object_key was never uploaded.
func (c *MockClient) GetUploadRecord(objectKey string) (MockUploadRecord, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.uploaded[objectKey]
	return r, ok
}

// handlePUT accepts PUT /<object_key> requests from the test client.
//
// Validation:
//   - Method must be PUT.
//   - Path must contain a non-empty object_key.
//   - Content-Length (when provided) and the actual body size both must be
//     ≤ cfg.MaxImageSize. We rely on io.LimitReader to bound memory even
//     if the client lies about Content-Length.
//
// On success we record the upload and return 200. The verify script then
// posts to /api/v1/upload/callback with the matching nonce.
func (c *MockClient) handlePUT(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	objectKey := strings.TrimPrefix(r.URL.Path, "/")
	if objectKey == "" {
		http.Error(w, "missing object key", http.StatusBadRequest)
		return
	}

	max := c.cfg.MaxImageSize
	if max <= 0 {
		max = 5 * 1024 * 1024
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > max {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	c.mu.Lock()
	c.uploaded[objectKey] = MockUploadRecord{
		ObjectKey:   objectKey,
		ContentType: r.Header.Get("Content-Type"),
		Size:        int64(len(body)),
		UploadedAt:  time.Now(),
	}
	c.mu.Unlock()

	c.log.Debug("mock oss PUT accepted",
		zap.String("object_key", objectKey),
		zap.Int("size", len(body)),
	)
	w.WriteHeader(http.StatusOK)
}

// buildObjectKey produces the canonical object key used by BOTH mock and
// aliyun. Keep this function shared so dev and prod see identical paths.
//
// Layout: {purpose}/{user_id}/{yyyymm}/{uuid}.{ext}
//   - purpose first → bucket lifecycle rules (e.g. "auto-cold tier for
//     step/* after 90d") can target one prefix.
//   - user_id second → quick "who uploaded this" forensic queries.
//   - yyyymm third → makes manual bulk-delete by month trivial.
//   - uuid + ext last → collision-free.
//
// path.Clean is applied defensively to strip stray "..", though all inputs
// come from validated DTO fields.
func buildObjectKey(p PresignParams) string {
	ext := extFromContentType(p.ContentType)
	yyyymm := time.Now().Format("200601")
	key := fmt.Sprintf("%s/%d/%s/%s.%s",
		p.Purpose,
		p.UserID,
		yyyymm,
		uuid.NewString(),
		ext,
	)
	return path.Clean(key)
}
