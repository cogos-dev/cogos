// .cog/salience.go
// Git-Derived Salience System
//
// Implements ADR-018: Salience System (Git-Derived Attention)
//
// This replaces .cog/lib/salience.sh with a native Go implementation
// using go-git for efficient git history parsing without subprocess spawning.
//
// Performance target: <5ms per file (vs 80ms in shell version)

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// === CONFIGURATION ===

// SalienceConfig holds configuration for salience computation
type SalienceConfig struct {
	WeightRecency    float64
	WeightFrequency  float64
	WeightChurn      float64
	WeightAuthorship float64
	DecayModel       string
	HalfLife         int // in days
}

// DefaultSalienceConfig returns the default configuration
func DefaultSalienceConfig() *SalienceConfig {
	return &SalienceConfig{
		WeightRecency:    0.4,
		WeightFrequency:  0.3,
		WeightChurn:      0.2,
		WeightAuthorship: 0.1,
		DecayModel:       "exponential",
		HalfLife:         30,
	}
}

// LoadSalienceConfigFromEnv loads config from environment variables
func LoadSalienceConfigFromEnv() *SalienceConfig {
	cfg := DefaultSalienceConfig()

	if val := os.Getenv("COG_SALIENCE_WEIGHT_RECENCY"); val != "" {
		fmt.Sscanf(val, "%f", &cfg.WeightRecency)
	}
	if val := os.Getenv("COG_SALIENCE_WEIGHT_FREQUENCY"); val != "" {
		fmt.Sscanf(val, "%f", &cfg.WeightFrequency)
	}
	if val := os.Getenv("COG_SALIENCE_WEIGHT_CHURN"); val != "" {
		fmt.Sscanf(val, "%f", &cfg.WeightChurn)
	}
	if val := os.Getenv("COG_SALIENCE_WEIGHT_AUTHORSHIP"); val != "" {
		fmt.Sscanf(val, "%f", &cfg.WeightAuthorship)
	}
	if val := os.Getenv("COG_SALIENCE_DECAY"); val != "" {
		cfg.DecayModel = val
	}
	if val := os.Getenv("COG_SALIENCE_HALFLIFE"); val != "" {
		fmt.Sscanf(val, "%d", &cfg.HalfLife)
	}

	return cfg
}

// === DATA STRUCTURES ===

// SalienceScore represents the computed salience for a file
type SalienceScore struct {
	Recency      float64
	Frequency    float64
	Churn        float64
	Authorship   float64
	Total        float64
	CommitCount  int
	TotalChanges int
	UniqueAuthors int
	DaysAgo      int
}

// FileScore pairs a file path with its salience score
type FileScore struct {
	Path  string
	Score float64
}

// === DECAY MODELS ===

// computeDecay calculates time decay based on the configured model
func computeDecay(model string, daysAgo int, halfLife int) float64 {
	if daysAgo < 0 {
		daysAgo = 0
	}

	switch model {
	case "exponential":
		// e^(-t/τ)
		return math.Exp(-float64(daysAgo) / float64(halfLife))

	case "linear":
		// max(0, 1 - t/(2τ))
		v := 1.0 - float64(daysAgo)/(2.0*float64(halfLife))
		if v < 0 {
			return 0
		}
		return v

	case "step":
		// t < τ ? 1 : 0
		if daysAgo < halfLife {
			return 1.0
		}
		return 0.0

	case "logarithmic":
		// 1 / (1 + log(1 + t/τ))
		t := 1.0 + float64(daysAgo)/float64(halfLife)
		return 1.0 / (1.0 + math.Log(t))

	default:
		return 0.0
	}
}

// === CORE SALIENCE COMPUTATION ===

// ComputeFileSalience computes salience for a single file
// Returns nil if file has no git history or doesn't exist
func ComputeFileSalience(repoPath string, filePath string, daysWindow int, cfg *SalienceConfig) (*SalienceScore, error) {
	// Verify file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("file not found: %s", filePath)
	}

	// Churn calculation is expensive; allow disabling
	enableChurn := false
	if val := os.Getenv("COG_SALIENCE_CHURN"); val != "" {
		lower := strings.ToLower(val)
		if lower == "1" || lower == "true" || lower == "yes" {
			enableChurn = true
		}
	}

	// Normalize to repo-relative path for git operations
	relPath := filePath
	if filepath.IsAbs(filePath) {
		if rel, err := filepath.Rel(repoPath, filePath); err == nil {
			cleanRel := filepath.Clean(rel)
			if cleanRel != ".." && !strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
				relPath = cleanRel
			}
		}
	}
	relPath = filepath.ToSlash(relPath)

	// Open git repository
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repo: %w", err)
	}

	// Calculate cutoff time
	cutoffTime := time.Now().AddDate(0, 0, -daysWindow)

	// Get commit iterator
	commitIter, err := repo.Log(&git.LogOptions{
		PathFilter: func(path string) bool {
			return path == relPath
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get git log: %w", err)
	}
	defer commitIter.Close()

	// Parse commits
	var (
		commitCount   int
		totalChanges  int
		lastTimestamp time.Time
		authors       = make(map[string]bool)
	)

	err = commitIter.ForEach(func(c *object.Commit) error {
		// Skip commits older than window
		if c.Author.When.Before(cutoffTime) {
			return storer.ErrStop
		}

		commitCount++
		authors[c.Author.Email] = true

		// Track most recent commit
		if lastTimestamp.IsZero() || c.Author.When.After(lastTimestamp) {
			lastTimestamp = c.Author.When
		}

		// Calculate changes (churn)
		// Get file stats from commit
		if enableChurn {
			stats, err := c.Stats()
			if err == nil {
				for _, stat := range stats {
					if stat.Name == relPath {
						totalChanges += stat.Addition + stat.Deletion
						break
					}
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error iterating commits: %w", err)
	}

	// If no commits found, return zero salience
	if commitCount == 0 {
		return &SalienceScore{
			Recency:      0,
			Frequency:    0,
			Churn:        0,
			Authorship:   0,
			Total:        0,
			CommitCount:  0,
			TotalChanges: 0,
			UniqueAuthors: 0,
			DaysAgo:      0,
		}, nil
	}

	// Compute metrics
	now := time.Now()
	daysAgo := int(now.Sub(lastTimestamp).Hours() / 24)

	// Recency: use decay model
	recency := computeDecay(cfg.DecayModel, daysAgo, cfg.HalfLife)

	// Frequency: commits/10, max 1.0
	frequency := float64(commitCount) / 10.0
	if frequency > 1.0 {
		frequency = 1.0
	}

	// Churn: avg_changes/100, max 1.0
	avgChanges := float64(totalChanges) / float64(commitCount)
	churn := avgChanges / 100.0
	if churn > 1.0 {
		churn = 1.0
	}

	// Authorship: unique authors/5, max 1.0
	uniqueAuthors := len(authors)
	authorship := float64(uniqueAuthors) / 5.0
	if authorship > 1.0 {
		authorship = 1.0
	}

	// Combined score with configurable weights
	total := cfg.WeightRecency*recency +
		cfg.WeightFrequency*frequency +
		cfg.WeightChurn*churn +
		cfg.WeightAuthorship*authorship

	return &SalienceScore{
		Recency:       recency,
		Frequency:     frequency,
		Churn:         churn,
		Authorship:    authorship,
		Total:         total,
		CommitCount:   commitCount,
		TotalChanges:  totalChanges,
		UniqueAuthors: uniqueAuthors,
		DaysAgo:       daysAgo,
	}, nil
}

// === RANKING AND FILTERING ===

// RankFilesBySalience ranks all markdown files in a scope by salience
func RankFilesBySalience(repoPath string, scope string, limit int, daysWindow int, cfg *SalienceConfig) ([]FileScore, error) {
	var scores []FileScore

	// Find all markdown files in scope
	err := filepath.Walk(scope, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Only process .md and .cog.md files
		ext := filepath.Ext(path)
		if ext != ".md" && filepath.Ext(path[:len(path)-len(ext)]) != ".cog" {
			return nil
		}

		// Compute salience
		score, err := ComputeFileSalience(repoPath, path, daysWindow, cfg)
		if err != nil {
			// Skip files with errors (likely no git history)
			return nil
		}

		if score != nil {
			scores = append(scores, FileScore{
				Path:  path,
				Score: score.Total,
			})
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error walking directory: %w", err)
	}

	// Sort by score descending
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})

	// Limit results
	if limit > 0 && len(scores) > limit {
		scores = scores[:limit]
	}

	return scores, nil
}

// GetHotFiles returns files with salience above threshold
func GetHotFiles(repoPath string, scope string, limit int, threshold float64, daysWindow int, cfg *SalienceConfig) ([]string, error) {
	// Get ranked files (request more than limit to filter by threshold)
	ranked, err := RankFilesBySalience(repoPath, scope, 100, daysWindow, cfg)
	if err != nil {
		return nil, err
	}

	var hot []string
	for _, fs := range ranked {
		if fs.Score > threshold {
			hot = append(hot, fs.Path)
			if limit > 0 && len(hot) >= limit {
				break
			}
		}
	}

	return hot, nil
}

// GetColdFiles returns files with low salience
func GetColdFiles(repoPath string, scope string, limit int, threshold float64, daysWindow int, cfg *SalienceConfig) ([]string, error) {
	// Get all ranked files
	ranked, err := RankFilesBySalience(repoPath, scope, 0, daysWindow, cfg)
	if err != nil {
		return nil, err
	}

	var cold []string
	// Collect files below threshold (excluding zero scores)
	for _, fs := range ranked {
		if fs.Score > 0 && fs.Score < threshold {
			cold = append(cold, fs.Path)
		}
	}

	// Return last N files (coldest)
	if limit > 0 && len(cold) > limit {
		cold = cold[len(cold)-limit:]
	}

	return cold, nil
}

// GetStaleFiles returns files with zero salience
func GetStaleFiles(repoPath string, scope string, daysWindow int, cfg *SalienceConfig) ([]string, error) {
	var stale []string

	err := filepath.Walk(scope, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Only process .md and .cog.md files
		ext := filepath.Ext(path)
		if ext != ".md" && filepath.Ext(path[:len(path)-len(ext)]) != ".cog" {
			return nil
		}

		score, err := ComputeFileSalience(repoPath, path, daysWindow, cfg)
		if err != nil || score == nil || score.Total == 0.0 {
			stale = append(stale, path)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return stale, nil
}

// === WORKSPACE HEALTH ===

// HealthStats represents workspace health statistics
type HealthStats struct {
	Total    int
	Hot      int // >= 0.7
	Warm     int // 0.3-0.7
	Cold     int // 0.1-0.3
	Stale    int // 0.0
	Activity int // percentage
	TopHot   []FileScore
	TopCold  []FileScore
}

// ComputeHealthStats generates workspace health report
func ComputeHealthStats(repoPath string, scope string, daysWindow int, cfg *SalienceConfig) (*HealthStats, error) {
	// Count total files
	var total int
	filepath.Walk(scope, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".md" || filepath.Ext(path[:len(path)-len(ext)]) == ".cog" {
			total++
		}
		return nil
	})

	// Get all ranked files
	ranked, err := RankFilesBySalience(repoPath, scope, 0, daysWindow, cfg)
	if err != nil {
		return nil, err
	}

	stats := &HealthStats{
		Total: total,
	}

	// Categorize files
	for _, fs := range ranked {
		if fs.Score >= 0.7 {
			stats.Hot++
		} else if fs.Score >= 0.3 {
			stats.Warm++
		} else if fs.Score >= 0.1 {
			stats.Cold++
		}
	}

	stats.Stale = total - (stats.Hot + stats.Warm + stats.Cold)
	if stats.Stale < 0 {
		stats.Stale = 0
	}

	if total > 0 {
		stats.Activity = int(float64(stats.Hot+stats.Warm) / float64(total) * 100)
	}

	// Get top hot files (top 5)
	if len(ranked) > 5 {
		stats.TopHot = ranked[:5]
	} else {
		stats.TopHot = ranked
	}

	// Get top cold files (bottom 5 with score > 0)
	var coldFiles []FileScore
	for _, fs := range ranked {
		if fs.Score > 0 && fs.Score < 0.3 {
			coldFiles = append(coldFiles, fs)
		}
	}
	if len(coldFiles) > 5 {
		stats.TopCold = coldFiles[:5]
	} else {
		stats.TopCold = coldFiles
	}

	return stats, nil
}

// === CLI COMMANDS ===

// cmdSalienceFile shows salience for a single file
func cmdSalienceFile(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog salience file <path> [days_window]")
	}

	filePath := args[0]
	daysWindow := 90
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	score, err := ComputeFileSalience(repoPath, filePath, daysWindow, cfg)
	if err != nil {
		return err
	}

	if score == nil {
		fmt.Println("0.00")
		return nil
	}

	fmt.Printf("%.2f\n", score.Total)
	return nil
}

// cmdSalienceMetrics shows detailed metrics for a file
func cmdSalienceMetrics(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog salience metrics <path> [days_window]")
	}

	filePath := args[0]
	daysWindow := 90
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	score, err := ComputeFileSalience(repoPath, filePath, daysWindow, cfg)
	if err != nil {
		return err
	}

	cogRoot := getWorkspaceRoot()

	if score == nil || score.Total == 0.0 {
		fmt.Printf("No git history for: %s\n", PathToURI(cogRoot, filePath))
		fmt.Println("  Salience: 0.00")
		return nil
	}

	fmt.Printf("Salience metrics for: %s\n", PathToURI(cogRoot, filePath))
	fmt.Printf("  Salience:   %.2f\n", score.Total)
	fmt.Println("  ----------------------------------------")
	fmt.Printf("  Recency:    %.2f (%dd ago, weight: %.2f)\n", score.Recency, score.DaysAgo, cfg.WeightRecency)
	fmt.Printf("  Frequency:  %.2f (%d commits, weight: %.2f)\n", score.Frequency, score.CommitCount, cfg.WeightFrequency)
	fmt.Printf("  Churn:      %.2f (avg %d lines/commit, weight: %.2f)\n", score.Churn, score.TotalChanges/score.CommitCount, cfg.WeightChurn)
	fmt.Printf("  Authorship: %.2f (%d authors, weight: %.2f)\n", score.Authorship, score.UniqueAuthors, cfg.WeightAuthorship)
	fmt.Println("  ----------------------------------------")
	fmt.Printf("  Commits:    %d\n", score.CommitCount)
	fmt.Printf("  Changes:    %d lines\n", score.TotalChanges)
	fmt.Printf("  Authors:    %d unique\n", score.UniqueAuthors)
	fmt.Printf("  Model:      %s (τ=%d days)\n", cfg.DecayModel, cfg.HalfLife)

	return nil
}

// cmdSalienceRank ranks files by salience
func cmdSalienceRank(args []string) error {
	scope := ".cog"
	limit := 20
	daysWindow := 90

	if len(args) > 0 {
		scope = args[0]
	}
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &limit)
	}
	if len(args) > 2 {
		fmt.Sscanf(args[2], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	cogRoot := getWorkspaceRoot()

	ranked, err := RankFilesBySalience(repoPath, scope, limit, daysWindow, cfg)
	if err != nil {
		return err
	}

	for _, fs := range ranked {
		fmt.Printf("%.2f %s\n", fs.Score, PathToURI(cogRoot, fs.Path))
	}

	return nil
}

// cmdSalienceHot shows hot files
func cmdSalienceHot(args []string) error {
	scope := ".cog"
	limit := 10
	threshold := 0.5
	daysWindow := 90

	if len(args) > 0 {
		scope = args[0]
	}
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &limit)
	}
	if len(args) > 2 {
		fmt.Sscanf(args[2], "%f", &threshold)
	}
	if len(args) > 3 {
		fmt.Sscanf(args[3], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	cogRoot := getWorkspaceRoot()

	hot, err := GetHotFiles(repoPath, scope, limit, threshold, daysWindow, cfg)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "=== Hot Files in %s (%d files) ===\n", scope, len(hot))
	for _, path := range hot {
		fmt.Println(PathToURI(cogRoot, path))
	}

	return nil
}

// cmdSalienceCold shows cold files
func cmdSalienceCold(args []string) error {
	scope := ".cog"
	limit := 10
	threshold := 0.1
	daysWindow := 90

	if len(args) > 0 {
		scope = args[0]
	}
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &limit)
	}
	if len(args) > 2 {
		fmt.Sscanf(args[2], "%f", &threshold)
	}
	if len(args) > 3 {
		fmt.Sscanf(args[3], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	cogRoot := getWorkspaceRoot()

	cold, err := GetColdFiles(repoPath, scope, limit, threshold, daysWindow, cfg)
	if err != nil {
		return err
	}

	for _, path := range cold {
		fmt.Println(PathToURI(cogRoot, path))
	}

	return nil
}

// cmdSalienceStale shows stale files
func cmdSalienceStale(args []string) error {
	scope := ".cog"
	daysWindow := 90

	if len(args) > 0 {
		scope = args[0]
	}
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	cogRoot := getWorkspaceRoot()

	stale, err := GetStaleFiles(repoPath, scope, daysWindow, cfg)
	if err != nil {
		return err
	}

	for _, path := range stale {
		fmt.Println(PathToURI(cogRoot, path))
	}

	return nil
}

// cmdSalienceHealth shows workspace health report
func cmdSalienceHealth(args []string) error {
	scope := ".cog"
	daysWindow := 90

	if len(args) > 0 {
		scope = args[0]
	}
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &daysWindow)
	}

	repoPath := ".."
	cfg := LoadSalienceConfigFromEnv()

	cogRoot := getWorkspaceRoot()

	stats, err := ComputeHealthStats(repoPath, scope, daysWindow, cfg)
	if err != nil {
		return err
	}

	fmt.Println("=== Salience Health Report ===")
	fmt.Println()
	fmt.Printf("Total:  %d\n", stats.Total)
	fmt.Printf("Hot:    %d (>= 0.7)\n", stats.Hot)
	fmt.Printf("Warm:   %d (0.3-0.7)\n", stats.Warm)
	fmt.Printf("Cold:   %d (0.1-0.3)\n", stats.Cold)
	fmt.Printf("Stale:  %d (0.0)\n", stats.Stale)
	fmt.Println()
	fmt.Printf("Activity: %d%%\n", stats.Activity)
	fmt.Println()

	fmt.Println("=== Top 5 Hot Files ===")
	for _, fs := range stats.TopHot {
		fmt.Printf("%.2f %s\n", fs.Score, PathToURI(cogRoot, fs.Path))
	}

	fmt.Println()
	fmt.Println("=== Top 5 Cold Files ===")
	for _, fs := range stats.TopCold {
		fmt.Printf("%.2f %s\n", fs.Score, PathToURI(cogRoot, fs.Path))
	}

	return nil
}
