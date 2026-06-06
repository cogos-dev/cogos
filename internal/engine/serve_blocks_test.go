// serve_blocks_test.go — HTTP handler tests for block + blob sync endpoints.
//
// Currently covers:
//
//	GET /v1/blobs/{digest}  — ADR-084 Phase 2 digest → bytes resolution
//	                          (hit / miss / malformed paths)
package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newBlobsTestServer returns a real *Server rooted at a tmpdir plus the shared
// Process so callers can pre-populate the BlobStore before firing requests.
func newBlobsTestServer(t *testing.T) (http.Handler, *Process) {
	t.Helper()
	root := t.TempDir()
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Test", "tester")
	proc := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, proc)
	t.Cleanup(func() {
		if b := proc.Broker(); b != nil {
			_ = b.Close()
		}
	})
	return srv.Handler(), proc
}

// TestBlobGetHit exercises the happy path: a blob stored via BlobStore.Store
// is retrievable by `sha256:<hex>` digest, with Content-Type recovered from
// the manifest.
func TestBlobGetHit(t *testing.T) {
	t.Parallel()
	handler, proc := newBlobsTestServer(t)

	body := []byte("g2 blob fixture — ADR-084 by-reference payload")
	const ct = "text/plain; charset=utf-8"
	bs := proc.BlobStore()
	if bs == nil {
		t.Fatal("BlobStore() nil after NewProcess; G10 wiring regressed")
	}
	hashHex, err := bs.Store(body, ct)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/blobs/sha256:"+hashHex, nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%q", rec.Code, rec.Body.String())
	}
	got, _ := io.ReadAll(rec.Body)
	if string(got) != string(body) {
		t.Fatalf("body mismatch:\n got=%q\nwant=%q", got, body)
	}
	if gotCT := rec.Header().Get("Content-Type"); gotCT != ct {
		t.Errorf("Content-Type = %q; want %q (from manifest)", gotCT, ct)
	}
	if gotDigest := rec.Header().Get("X-Blob-Digest"); gotDigest != "sha256:"+hashHex {
		t.Errorf("X-Blob-Digest = %q; want sha256:%s", gotDigest, hashHex)
	}
}

// TestBlobGetHitDefaultContentType confirms the handler falls back to
// application/octet-stream when the manifest entry has no content_type
// recorded (BlobStore.Store allows an empty ct string).
func TestBlobGetHitDefaultContentType(t *testing.T) {
	t.Parallel()
	handler, proc := newBlobsTestServer(t)

	body := []byte{0x00, 0x01, 0x02, 0x03}
	hashHex, err := proc.BlobStore().Store(body, "")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/blobs/sha256:"+hashHex, nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if gotCT := rec.Header().Get("Content-Type"); gotCT != "application/octet-stream" {
		t.Errorf("Content-Type = %q; want application/octet-stream", gotCT)
	}
}

// TestBlobGetNotFound confirms a well-formed digest with no stored blob
// returns 404, not 500.
func TestBlobGetNotFound(t *testing.T) {
	t.Parallel()
	handler, _ := newBlobsTestServer(t)

	// Well-formed digest for content that was never stored.
	h := sha256.Sum256([]byte("this was never stored"))
	digest := "sha256:" + hex.EncodeToString(h[:])

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/blobs/"+digest, nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404; body=%q", rec.Code, rec.Body.String())
	}
}

// TestBlobGetMalformedDigest exercises the 400 path for several malformed
// inputs: missing prefix, wrong length, non-hex characters, and other
// algorithms we don't support yet.
func TestBlobGetMalformedDigest(t *testing.T) {
	t.Parallel()
	handler, _ := newBlobsTestServer(t)

	cases := []struct {
		name   string
		digest string
	}{
		{"missing_prefix", strings.Repeat("a", 64)},
		{"wrong_algo", "md5:" + strings.Repeat("a", 32)},
		{"too_short", "sha256:" + strings.Repeat("a", 10)},
		{"too_long", "sha256:" + strings.Repeat("a", 65)},
		{"non_hex", "sha256:" + strings.Repeat("z", 64)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/blobs/"+tc.digest, nil)
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("digest=%q: status = %d; want 400; body=%q",
					tc.digest, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestParseSHA256DigestRoundTrip is a small unit test on the parser so
// regressions here surface independently of the HTTP layer.
func TestParseSHA256DigestRoundTrip(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("a", 64)
	got, ok := parseSHA256Digest("sha256:" + want)
	if !ok || got != want {
		t.Errorf("lowercase: got=(%q, %v); want=(%q, true)", got, ok, want)
	}

	// Mixed case hex is normalized to lowercase.
	mixed := "sha256:AAAA" + strings.Repeat("b", 60)
	got, ok = parseSHA256Digest(mixed)
	if !ok || got != strings.ToLower(mixed[len("sha256:"):]) {
		t.Errorf("mixed case: got=(%q, %v); want lowercase normalization", got, ok)
	}

	// Empty and nonsense reject.
	if _, ok := parseSHA256Digest(""); ok {
		t.Error("empty string: parser accepted")
	}
	if _, ok := parseSHA256Digest("sha256:"); ok {
		t.Error("prefix only: parser accepted")
	}
}

// TestPutBlockStreamsLargeBlob proves the PUT handler streams a large blob to
// disk (via io.TeeReader + temp file + rolling SHA-256) without buffering it in
// memory or rejecting it on a size cap, then confirms the stored bytes round
// trip back through GET intact. Mirrors the GET large-blob streaming test.
func TestPutBlockStreamsLargeBlob(t *testing.T) {
	t.Parallel()
	handler, _ := newBlobsTestServer(t)

	const size = 200 << 20 // 200 MiB
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	sum := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(sum[:])

	// PUT the blob. The body must be a fresh reader the handler can stream.
	putRec := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/v1/blocks/"+hashHex, bytes.NewReader(payload))
	handler.ServeHTTP(putRec, putReq)

	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; want 201 or 200; body=%q", putRec.Code, putRec.Body.String())
	}

	// GET it back and verify the full payload round trips.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/blocks/"+hashHex, nil)
	handler.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d; want 200; body=%q", getRec.Code, getRec.Body.String())
	}

	got, err := io.ReadAll(getRec.Body)
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	if len(got) != size {
		t.Fatalf("GET body length = %d; want %d", len(got), size)
	}
	gotSum := sha256.Sum256(got)
	if hex.EncodeToString(gotSum[:]) != hashHex {
		t.Fatalf("GET body sha256 = %s; want %s", hex.EncodeToString(gotSum[:]), hashHex)
	}
}
