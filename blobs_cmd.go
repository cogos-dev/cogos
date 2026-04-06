// blobs_cmd.go — CLI commands for blob store management
//
// Usage:
//
//	cogos-v3 blobs list              — list all stored blobs
//	cogos-v3 blobs store <file>      — manually store a file
//	cogos-v3 blobs get <hash> <out>  — retrieve blob to file
//	cogos-v3 blobs verify            — check all pointers have matching blobs
//	cogos-v3 blobs gc [--dry-run]    — garbage collect unreferenced blobs
//	cogos-v3 blobs init              — initialize the blob store
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

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
			fmt.Fprintln(os.Stderr, "usage: cogos-v3 blobs store <file>")
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
			fmt.Fprintln(os.Stderr, "usage: cogos-v3 blobs get <hash> <output-file>")
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
			fmt.Fprintln(os.Stderr, "usage: cogos-v3 blobs dehydrate <file-or-dir>")
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

	default:
		fmt.Fprintf(os.Stderr, "unknown blobs subcommand: %s\n", args[0])
		printBlobsUsage()
		os.Exit(1)
	}
}

func printBlobsUsage() {
	fmt.Println(`Usage: cogos-v3 blobs <command>

Commands:
  init              Initialize the blob store
  list              List all stored blobs
  store <file>      Store a file in the blob store
  get <hash> <out>  Retrieve a blob to a file
  verify            Check all pointers have matching blobs
  gc [--dry-run]    Garbage collect unreferenced blobs
  hydrate           Restore blob content to original file paths
  dehydrate <path>  Replace large files with blob pointers`)
}
