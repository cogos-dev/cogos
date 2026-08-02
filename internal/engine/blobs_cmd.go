// blobs_cmd.go — CLI commands for blob store management
//
// Usage:
//
//	cogos blobs list              — list all stored blobs
//	cogos blobs store <file>      — manually store a file
//	cogos blobs get <hash> <out>  — retrieve blob to file
//	cogos blobs verify            — check all pointers have matching blobs
//	cogos blobs gc [--dry-run]    — garbage collect unreferenced blobs
//	cogos blobs init              — initialize the blob store
package engine

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ManifestShard is one content-addressed file entry in a model manifest.
type ManifestShard struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// ModelManifest is the cross-node contract for a model directory (A-style flat
// JSON): a model_id plus the content-addressed shards it comprises.
type ModelManifest struct {
	Type    string          `json:"type"`
	ModelID string          `json:"model_id"`
	Shards  []ManifestShard `json:"shards"`
}

// buildModelManifest walks a model directory and builds a ModelManifest. Each
// regular file becomes a shard {path (relative to dir), sha256 hex, size}. If
// modelID is empty, it defaults to filepath.Base(dir).
func buildModelManifest(dir, modelID string) (ModelManifest, error) {
	if modelID == "" {
		modelID = filepath.Base(filepath.Clean(dir))
	}
	manifest := ModelManifest{
		Type:    "model.manifest",
		ModelID: modelID,
		Shards:  []ManifestShard{},
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", path, rerr)
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return fmt.Errorf("rel %s: %w", path, rerr)
		}
		// Normalize to forward slashes so the manifest is portable across nodes.
		rel = filepath.ToSlash(rel)
		manifest.Shards = append(manifest.Shards, ManifestShard{
			Path: rel,
			Hash: hashBytes(content),
			Size: int64(len(content)),
		})
		return nil
	})
	if err != nil {
		return ModelManifest{}, err
	}
	return manifest, nil
}

// normalizeNodeURL prepends http:// when the node has no scheme (e.g. a bare
// host:port like "node-a:6931").
func normalizeNodeURL(node string) string {
	if !strings.Contains(node, "://") {
		return "http://" + node
	}
	return node
}

func runBlobsCmd(args []string, workspace string) {
	if workspace == "" {
		wd, _ := os.Getwd()
		ws, err := findWorkspaceRoot(wd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not detect workspace: %v\n", err)
			os.Exit(1)
		}
		workspace = ws
	}

	bs := NewBlobStore(workspace)

	if len(args) == 0 {
		printBlobsUsage()
		return
	}

	switch args[0] {
	case "init":
		if err := bs.Init(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Blob store initialized at .cog/blobs/")

	case "list", "ls":
		if err := bs.PrintBlobList(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

	case "store":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: cogos blobs store <file>")
			os.Exit(1)
		}
		filePath := args[1]
		info, err := os.Stat(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		if err := bs.Init(); err != nil {
			fmt.Fprintf(os.Stderr, "error: init: %v\n", err)
			os.Exit(1)
		}

		ct := ContentTypeFromExt(filePath)
		hash, err := bs.StoreFile(filePath, ct)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Stored: %s\n", hash[:12])
		fmt.Printf("  hash: %s\n", hash)
		fmt.Printf("  size: %s\n", humanSize(info.Size()))
		fmt.Printf("  type: %s\n", ct)
		fmt.Printf("  path: .cog/blobs/%s/%s\n", hash[:2], hash[2:])

	case "get":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: cogos blobs get <hash> <output-file>")
			os.Exit(1)
		}
		hash := args[1]
		outPath := args[2]

		content, err := bs.Get(hash)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		if err := os.WriteFile(outPath, content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write %s: %v\n", outPath, err)
			os.Exit(1)
		}
		fmt.Printf("Retrieved %s → %s (%s)\n", hash[:12], outPath, humanSize(int64(len(content))))

	case "verify":
		missing, err := bs.Verify(workspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if len(missing) == 0 {
			fmt.Println("All blob pointers have matching blobs.")
		} else {
			fmt.Printf("%d blob(s) missing:\n", len(missing))
			for _, h := range missing {
				fmt.Printf("  %s\n", h[:12])
			}
			os.Exit(1)
		}

	case "gc":
		dryRun := len(args) > 1 && args[1] == "--dry-run"

		refs, err := CollectReferencedHashes(workspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		if dryRun {
			entries, err := bs.List()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			wouldRemove := 0
			var wouldFree int64
			for _, e := range entries {
				if !refs[e.Hash] {
					wouldRemove++
					wouldFree += e.Size
					fmt.Printf("  would remove: %s (%s)\n", e.Hash[:12], humanSize(e.Size))
				}
			}
			fmt.Printf("\nDry run: %d blob(s) would be removed, %s would be freed\n",
				wouldRemove, humanSize(wouldFree))
			return
		}

		removed, freed, err := bs.GC(refs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Garbage collected: %d blob(s) removed, %s freed\n", removed, humanSize(freed))

	case "hydrate":
		// Restore blob content to original paths for all pointers.
		pointers, err := FindBlobPointers(workspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		hydrated := 0
		for _, p := range pointers {
			if p.OriginalPath == "" {
				continue
			}
			absPath := p.OriginalPath
			if !filepath.IsAbs(absPath) {
				absPath = filepath.Join(workspace, absPath)
			}
			// Skip if file already exists at original path.
			if _, err := os.Stat(absPath); err == nil {
				continue
			}
			content, err := bs.Get(p.Hash)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  missing: %s (%s)\n", p.Hash[:12], p.OriginalPath)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "  error: mkdir %s: %v\n", filepath.Dir(absPath), err)
				continue
			}
			if err := os.WriteFile(absPath, content, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "  error: write %s: %v\n", absPath, err)
				continue
			}
			hydrated++
			fmt.Printf("  hydrated: %s → %s\n", p.Hash[:12], p.OriginalPath)
		}
		fmt.Printf("\nHydrated %d blob(s) from %d pointer(s)\n", hydrated, len(pointers))

	case "dehydrate":
		// Replace files with blob pointers (store content, write pointer).
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: cogos blobs dehydrate <file-or-dir>")
			os.Exit(1)
		}
		target := args[1]
		if err := bs.Init(); err != nil {
			fmt.Fprintf(os.Stderr, "error: init: %v\n", err)
			os.Exit(1)
		}

		dehydrated := 0
		_ = filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if !ShouldRedirectToBlob(path, info.Size()) {
				return nil
			}

			ct := ContentTypeFromExt(path)
			hash, err := bs.StoreFile(path, ct)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error storing %s: %v\n", path, err)
				return nil
			}

			// Replace file with pointer.
			pointerPath := path + ".pointer.cog.md"
			relPath, _ := filepath.Rel(workspace, path)
			if err := bs.WritePointer(pointerPath, hash, info.Size(), ct, relPath); err != nil {
				fmt.Fprintf(os.Stderr, "  error writing pointer: %v\n", err)
				return nil
			}

			// Remove original file.
			_ = os.Remove(path)
			dehydrated++
			fmt.Printf("  dehydrated: %s → %s (%s)\n", filepath.Base(path), hash[:12], humanSize(info.Size()))
			return nil
		})
		fmt.Printf("\nDehydrated %d file(s)\n", dehydrated)

	case "manifest":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: cogos blobs manifest <model-dir> [--model-id <id>]")
			os.Exit(1)
		}
		modelDir := args[1]
		modelID := ""
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--model-id":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "error: --model-id requires a value")
					os.Exit(1)
				}
				modelID = args[i+1]
				i++
			default:
				fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
				os.Exit(1)
			}
		}

		info, err := os.Stat(modelDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if !info.IsDir() {
			fmt.Fprintf(os.Stderr, "error: %s is not a directory\n", modelDir)
			os.Exit(1)
		}

		manifest, err := buildModelManifest(modelDir, modelID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		out, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: marshal manifest: %v\n", err)
			os.Exit(1)
		}
		// ONLY the JSON goes to stdout so this can be redirected to a file.
		fmt.Println(string(out))

	case "remote-hydrate":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: cogos blobs remote-hydrate <manifest.json> --from <node> [--target <dir>] [--promote]")
			os.Exit(1)
		}
		manifestPath := args[1]
		fromNode := ""
		targetDir := ""
		promote := false
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--from":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "error: --from requires a value")
					os.Exit(1)
				}
				fromNode = args[i+1]
				i++
			case "--target":
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "error: --target requires a value")
					os.Exit(1)
				}
				targetDir = args[i+1]
				i++
			case "--promote":
				promote = true
			default:
				fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
				os.Exit(1)
			}
		}
		if fromNode == "" {
			fmt.Fprintln(os.Stderr, "error: --from <node> is required")
			os.Exit(1)
		}
		fromURL := normalizeNodeURL(fromNode)

		data, err := os.ReadFile(manifestPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read manifest: %v\n", err)
			os.Exit(1)
		}
		var manifest ModelManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			fmt.Fprintf(os.Stderr, "error: parse manifest: %v\n", err)
			os.Exit(1)
		}

		shards := make([]ModelShard, 0, len(manifest.Shards))
		for _, s := range manifest.Shards {
			shards = append(shards, ModelShard{RelPath: s.Path, Hash: s.Hash})
		}

		if err := bs.Init(); err != nil {
			fmt.Fprintf(os.Stderr, "error: init: %v\n", err)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "Hydrating %q (%d shards) from %s ...\n", manifest.ModelID, len(shards), fromURL)
		rep, err := RemoteHydrate(bs, fromURL, shards, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: remote-hydrate: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Remote hydrate report:\n")
		fmt.Printf("  shards total:  %d\n", rep.ShardsTotal)
		fmt.Printf("  already local: %d\n", rep.AlreadyLocal)
		fmt.Printf("  pulled:        %d\n", rep.Pulled)
		fmt.Printf("  deduped:       %d\n", rep.Deduped)
		fmt.Printf("  bytes pulled:  %s\n", humanSize(rep.BytesPulled))
		fmt.Printf("  elapsed:       %s\n", rep.Elapsed)

		if targetDir != "" {
			writeFile := func(path string, content []byte) error {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				return os.WriteFile(path, content, 0o644)
			}
			if err := MaterializeModelDir(bs, targetDir, shards, writeFile); err != nil {
				fmt.Fprintf(os.Stderr, "error: materialize: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Materialized %d shard(s) into %s\n", len(shards), targetDir)
		}

		if promote {
			promoted, err := Promote(bs, fromURL, shards)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: promote: %v\n", err)
				os.Exit(1)
			}
			if promoted {
				fmt.Printf("Self-promoted to LOCAL AUTHORITY (remote %s unreachable).\n", fromURL)
				fmt.Printf("  WARNING: split-brain risk — MANUAL RECONCILE REQUIRED on reconnect.\n")
			} else {
				fmt.Printf("Not promoted: remote %s is reachable; deferring to its authority.\n", fromURL)
			}
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown blobs subcommand: %s\n", args[0])
		printBlobsUsage()
		os.Exit(1)
	}
}

func printBlobsUsage() {
	fmt.Println(`Usage: cogos blobs <command>

Commands:
  init              Initialize the blob store
  list              List all stored blobs
  store <file>      Store a file in the blob store
  get <hash> <out>  Retrieve a blob to a file
  verify            Check all pointers have matching blobs
  gc [--dry-run]    Garbage collect unreferenced blobs
  hydrate           Restore blob content to original file paths
  dehydrate <path>  Replace large files with blob pointers
  manifest <model-dir> [--model-id <id>]
                    Emit a model.manifest JSON (shard path/hash/size) to stdout
  remote-hydrate <manifest.json> --from <node> [--target <dir>] [--promote]
                    Pull missing blocks from a remote node, optionally
                    materialize into --target and self-promote on partition`)
}
