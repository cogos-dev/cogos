// remote_hydrate_spike.go — SPIKE (spike-model-weight-block-cache)
//
// Gap-fill for the model-weight block cache: the cross-node leg that
// `cogos blobs hydrate` is missing. `hydrate` materializes from the LOCAL
// blob store only; this adds "when a blob is absent locally, fetch it from a
// remote node's /v1/blocks API before materializing."
//
// This is SPIKE code: a standalone client driving the REAL, already-shipped
// block-sync HTTP API (serve_blocks.go) and the REAL BlobStore (blobstore.go).
// Nothing here changes the kernel; it composes existing primitives to prove the
// reconcile loop the operator described:
//
//	Eclipse (authoritative) holds the model. Darkstar caches blocks by content
//	hash, holds a pointer not the authority, and on reconnect/drift pulls only
//	the delta. Self-reconcile falls out of content addressing.
//
// ADR-089 (pointer envelope) + ADR-084 (digest resolution) + ADR-069 (the
// distributed mesh, weights = the at-rest special case).
//
// Hardening pass (reviewed by Opus 4.8):
//   - F6: pulls STREAM to a temp file via io.TeeReader→sha256 (no 5GB in RAM).
//   - F4: concurrent pulls of the same hash are deduplicated in-flight; the
//     second caller waits for the first instead of re-fetching.
//   - F5: the dedup path emits a bandwidth warning so it is observable.
//   - Self-promote V1: a reachability gate that marks local authority only when
//     the remote is unreachable (manual reconcile required on reconnect).
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ModelShard is one content-addressed file in a model directory (a safetensors
// shard, config.json, tokenizer, etc.). In production these come from the
// blob.pointer cogdocs; the spike passes them directly.
type ModelShard struct {
	RelPath string // path within the materialized model dir, e.g. "model-00001.safetensors"
	Hash    string // sha256 hex — the content address; this is what makes drift detection free
}

// HydrateReport captures what the reconcile actually did, for measurement.
type HydrateReport struct {
	ShardsTotal  int
	AlreadyLocal int      // cache hits — bytes already present, no transfer
	Pulled       int      // blobs fetched from the remote node
	Deduped      int      // blobs another concurrent call fetched while we waited (F4)
	BytesPulled  int64    // wire volume of the delta
	PulledHashes []string // which shards were transferred (the delta)
	Elapsed      time.Duration
}

// inFlightEntry coordinates concurrent pulls of the SAME hash. The first caller
// to LoadOrStore an entry becomes the fetcher; every other caller blocks on
// done, then re-checks the local store. This makes concurrent RemoteHydrate of
// the same model idempotent and avoids the 2N-fetch thundering herd (F4).
type inFlightEntry struct {
	done chan struct{}
	err  error
}

// inFlight maps shard hash → *inFlightEntry for pulls currently in progress.
var inFlight sync.Map

// RemoteHydrate reconciles a model manifest into the local BlobStore by pulling
// only the blocks the local store is missing from a remote node, then verifying
// integrity. This is the cross-node cache leg.
//
//	bs        — the LOCAL blob store (Darkstar's cache)
//	remoteURL — base URL of the authoritative node's daemon (Eclipse), e.g. http://192.168.10.191:6931
//	shards    — the model manifest (what the model dir should contain)
//
// Reconcile semantics: only locally-missing hashes are pulled. Re-running after
// the remote re-publishes a shard (new content → new hash → updated manifest)
// pulls only that shard. No coherence protocol; content addressing IS the
// coherence.
func RemoteHydrate(bs *BlobStore, remoteURL string, shards []ModelShard, client *http.Client) (*HydrateReport, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	start := time.Now()
	rep := &HydrateReport{ShardsTotal: len(shards)}

	// 1. Determine the reconcile delta: which shard hashes are missing LOCALLY.
	//    We ask our own store directly (Exists) rather than trusting the remote;
	//    the remote's /v1/blocks/verify reports the remote's view, which is the
	//    inverse of what we want. Local-missing = our delta to pull.
	var toPull []ModelShard
	for _, s := range shards {
		if bs.Exists(s.Hash) {
			rep.AlreadyLocal++
			continue
		}
		toPull = append(toPull, s)
	}

	// 2. Pull each missing blob, streaming to disk with rolling-hash integrity,
	//    deduplicating against any concurrent in-flight pull of the same hash.
	for _, s := range toPull {
		n, fetched, err := pullShard(bs, remoteURL, s, client)
		if err != nil {
			return rep, err
		}
		if fetched {
			rep.Pulled++
			rep.BytesPulled += n
			rep.PulledHashes = append(rep.PulledHashes, s.Hash)
		} else {
			// Another concurrent caller fetched this shard while we waited.
			rep.Deduped++
		}
	}

	rep.Elapsed = time.Since(start)
	return rep, nil
}

// pullShard fetches a single missing shard, coordinating with any concurrent
// pull of the same hash. Returns (bytesPulled, fetchedByUs, err). When
// fetchedByUs is false but err is nil, another concurrent caller fetched the
// blob (the F4 dedup path) and it is now present locally.
func pullShard(bs *BlobStore, remoteURL string, s ModelShard, client *http.Client) (int64, bool, error) {
	entry := &inFlightEntry{done: make(chan struct{})}
	actual, loaded := inFlight.LoadOrStore(s.Hash, entry)
	if loaded {
		// Someone else is already pulling this exact hash. Wait for them rather
		// than opening a second wire transfer for identical content (F4). Emit
		// the bandwidth-warning so the dedup path is observable (F5).
		other := actual.(*inFlightEntry)
		slog.Warn("remote-hydrate: deduplicated concurrent pull", "hash", s.Hash[:12])
		<-other.done
		if bs.Exists(s.Hash) {
			return 0, false, nil // dedup hit — the other caller landed it
		}
		// The other caller failed and the blob is still absent. Fall through and
		// fetch it ourselves rather than inheriting their failure silently.
		entry = &inFlightEntry{done: make(chan struct{})}
		actual, loaded = inFlight.LoadOrStore(s.Hash, entry)
		if loaded {
			other = actual.(*inFlightEntry)
			<-other.done
			if bs.Exists(s.Hash) {
				return 0, false, nil
			}
			if other.err != nil {
				return 0, false, other.err
			}
		}
	}

	// We own the fetch for this hash. Always publish completion and clear the
	// in-flight slot so a later RemoteHydrate can retry on transient failure.
	var n int64
	var err error
	defer func() {
		entry.err = err
		inFlight.Delete(s.Hash)
		close(entry.done)
	}()

	n, err = streamFetch(bs, remoteURL, s, client)
	if err != nil {
		return n, false, err
	}
	return n, true, nil
}

// streamFetch pulls one blob and STREAMS it to a temp file (F6) — the bytes
// never accumulate in memory, so a 5GB shard costs O(buffer) RAM, not O(shard).
// The wire is tee'd into a rolling SHA-256; the digest must equal the requested
// content address before we commit the temp file into the store.
func streamFetch(bs *BlobStore, remoteURL string, s ModelShard, client *http.Client) (int64, error) {
	url := strings.TrimRight(remoteURL, "/") + "/v1/blocks/" + s.Hash
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("pull %s: %w", s.Hash[:12], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("pull %s: remote status %d", s.Hash[:12], resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "remote-hydrate-*.blob")
	if err != nil {
		return 0, fmt.Errorf("temp %s: %w", s.Hash[:12], err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // StoreFile copies the bytes in; the temp is ours to clean.

	// Tee the download into a rolling hash as it streams to disk.
	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(resp.Body, h))
	closeErr := tmp.Close()
	if err != nil {
		return n, fmt.Errorf("stream %s: %w", s.Hash[:12], err)
	}
	if closeErr != nil {
		return n, fmt.Errorf("flush %s: %w", s.Hash[:12], closeErr)
	}

	// Content addressing: the streamed bytes MUST hash to the address we asked
	// for. If not, the remote is lying or corrupt — refuse before committing.
	got := hex.EncodeToString(h.Sum(nil))
	if got != s.Hash {
		return n, fmt.Errorf("integrity: asked %s got %s", s.Hash[:12], got[:12])
	}

	ct := resp.Header.Get("Content-Type")
	if _, err := bs.StoreFile(tmpPath, ct); err != nil {
		return n, fmt.Errorf("store %s: %w", s.Hash[:12], err)
	}
	return n, nil
}

// MaterializeModelDir writes the model's shards out to a loadable directory from
// the (now-complete) local blob store. This is the existing `hydrate` behavior,
// scoped to a single model manifest and a chosen output root. After this, an
// inference runtime can load the model from destDir; eviction later (GC) drops
// the bytes while the pointer/manifest survives for re-hydration.
func MaterializeModelDir(bs *BlobStore, destDir string, shards []ModelShard, writeFile func(path string, data []byte) error) error {
	for _, s := range shards {
		content, err := bs.Get(s.Hash) // Get verifies integrity on read
		if err != nil {
			return fmt.Errorf("materialize %s: %w", s.RelPath, err)
		}
		if err := writeFile(filepath.Join(destDir, s.RelPath), content); err != nil {
			return fmt.Errorf("write %s: %w", s.RelPath, err)
		}
	}
	return nil
}

// RemoteManifest fetches the remote node's full blob manifest. Useful for the
// "what does Eclipse have" discovery step. Composes the existing
// GET /v1/blocks/manifest endpoint.
func RemoteManifest(remoteURL string, client *http.Client) ([]BlobEntry, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := strings.TrimRight(remoteURL, "/") + "/v1/blocks/manifest"
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest: remote status %d", resp.StatusCode)
	}
	var out struct {
		Blobs []BlobEntry `json:"blobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Blobs, nil
}

// authorityMarker is the sentinel a cache writes when it self-promotes to local
// authority because the canonical remote is unreachable.
const authorityMarker = ".authority"

// Promote is the self-promote V1 reachability gate. A non-authoritative cache
// normally holds only a pointer, deferring to the remote authority. But if the
// remote is UNREACHABLE (partition, node down), the cache may temporarily mark
// itself authoritative so local readers are not blocked on a dead link.
//
// It probes remoteURL with a short-timeout TCP dial. Only when the remote is
// unreachable does it write a `.authority` sentinel into the blob store root and
// return promoted=true. When the remote is reachable it is a no-op (promoted=false):
// the real authority is alive, so we must NOT fork authority.
//
// CAUTION: self-promotion is a split-brain risk. On reconnect a human (or a
// higher-level reconcile job) MUST reconcile the two authorities — content
// addressing makes drift detectable but does not decide WHICH side wins when
// both mutated. Manual reconcile is required; this gate only buys availability
// during the partition.
func Promote(bs *BlobStore, remoteURL string, shards []ModelShard) (bool, error) {
	if remoteReachable(remoteURL, 2*time.Second) {
		// Authority is alive; defer to it. Clear any stale local-authority claim.
		_ = os.Remove(filepath.Join(bs.root, authorityMarker))
		return false, nil
	}

	// Remote is unreachable: mark local authority so readers can proceed.
	if err := os.MkdirAll(bs.root, 0o755); err != nil {
		return false, fmt.Errorf("promote: ensure store root: %w", err)
	}
	marker := filepath.Join(bs.root, authorityMarker)
	body := fmt.Sprintf("self-promoted at %s; remote %s unreachable; %d shards; MANUAL RECONCILE REQUIRED on reconnect\n",
		time.Now().UTC().Format(time.RFC3339), remoteURL, len(shards))
	if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
		return false, fmt.Errorf("promote: write authority marker: %w", err)
	}
	slog.Warn("remote-hydrate: self-promoted to local authority (remote unreachable)",
		"remote", remoteURL, "shards", len(shards))
	return true, nil
}

// remoteReachable returns true if a TCP connection to the remote URL's host:port
// succeeds within timeout. A bare HEAD would also work, but a raw dial is the
// cheapest liveness probe and does not require the remote to serve any route.
func remoteReachable(remoteURL string, timeout time.Duration) bool {
	u, err := url.Parse(remoteURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = net.JoinHostPort(u.Hostname(), "443")
		default:
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
