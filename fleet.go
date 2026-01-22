// Fleet Module - Agent fleet orchestration for parallel inference
//
// This module provides kubectl-style management of agent fleets:
// - spawn: Create and launch an agent fleet from a config
// - status: Check status of running fleets
// - reap: Collect results and clean up completed fleets
// - logs: View logs from a fleet
//
// The Go kernel handles orchestration, Python handles inference.

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// === FLEET CONFIGURATION ===

const (
	fleetDir           = ".cog/fleet"
	fleetRegistryFile  = ".cog/fleet/registry.json"
	fleetConfigsDir    = "projects/cog_lab_package/agents/fleet_configs"
	fleetRunnerModule  = "projects.cog_lab_package.lablib.fleet_runner"
)

// === FLEET STATE TYPES ===

// FleetState represents the state of a fleet
type FleetState string

const (
	FleetPending   FleetState = "pending"
	FleetRunning   FleetState = "running"
	FleetCompleted FleetState = "completed"
	FleetFailed    FleetState = "failed"
)

// FleetEntry represents a single fleet in the registry
type FleetEntry struct {
	ID         string     `json:"id"`
	Config     string     `json:"config"`
	Task       string     `json:"task"`
	State      FleetState `json:"state"`
	AgentCount int        `json:"agent_count"`
	Completed  int        `json:"completed"`
	Failed     int        `json:"failed"`
	CreatedAt  string     `json:"created_at"`
	UpdatedAt  string     `json:"updated_at"`
	ResultsDir string     `json:"results_dir"`
	PID        int        `json:"pid,omitempty"`
}

// FleetRegistry holds all fleet entries
type FleetRegistry struct {
	Version string                `json:"version"`
	Fleets  map[string]FleetEntry `json:"fleets"`
}

// FleetConfig represents a parsed fleet configuration
type FleetConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Models      []struct {
		ID    string `yaml:"id"`
		Role  string `yaml:"role"`
		Count int    `yaml:"count"`
	} `yaml:"models"`
}

// === REGISTRY MANAGEMENT ===

// loadFleetRegistry loads the fleet registry from disk
func loadFleetRegistry(root string) (*FleetRegistry, error) {
	registryPath := filepath.Join(root, fleetRegistryFile)

	data, err := os.ReadFile(registryPath)
	if err != nil {
		// Return empty registry if file doesn't exist
		return &FleetRegistry{
			Version: "1.0.0",
			Fleets:  make(map[string]FleetEntry),
		}, nil
	}

	var registry FleetRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("failed to parse fleet registry: %w", err)
	}

	if registry.Fleets == nil {
		registry.Fleets = make(map[string]FleetEntry)
	}

	return &registry, nil
}

// saveFleetRegistry saves the fleet registry to disk
func saveFleetRegistry(root string, registry *FleetRegistry) error {
	registryPath := filepath.Join(root, fleetRegistryFile)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(registryPath), 0755); err != nil {
		return fmt.Errorf("failed to create fleet directory: %w", err)
	}

	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal fleet registry: %w", err)
	}

	return writeAtomic(registryPath, data, 0644)
}

// generateFleetID creates a unique fleet identifier
func generateFleetID() string {
	bytes := make([]byte, 6)
	rand.Read(bytes)
	return "fleet_" + hex.EncodeToString(bytes)
}

// resolveFleetConfig finds a fleet config file by name
func resolveFleetConfig(root, name string) (string, error) {
	// Try exact path first
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}

	// Try in fleet_configs directory
	configPath := filepath.Join(root, fleetConfigsDir, name)
	if !strings.HasSuffix(configPath, ".yaml") {
		configPath += ".yaml"
	}

	if _, err := os.Stat(configPath); err == nil {
		return configPath, nil
	}

	return "", fmt.Errorf("fleet config not found: %s", name)
}

// === FLEET OPERATIONS ===

// spawnFleet creates and launches a new fleet
func spawnFleet(root, configName, task string) (*FleetEntry, error) {
	// Resolve config
	configPath, err := resolveFleetConfig(root, configName)
	if err != nil {
		return nil, err
	}

	// Generate fleet ID
	fleetID := generateFleetID()

	// Create fleet directory
	fleetPath := filepath.Join(root, fleetDir, fleetID)
	if err := os.MkdirAll(fleetPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create fleet directory: %w", err)
	}

	// Copy config to fleet directory
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(fleetPath, "config.yaml"), configData, 0644); err != nil {
		return nil, fmt.Errorf("failed to copy config: %w", err)
	}

	// Write task to file
	if err := os.WriteFile(filepath.Join(fleetPath, "task.txt"), []byte(task), 0644); err != nil {
		return nil, fmt.Errorf("failed to write task: %w", err)
	}

	// Create results directory
	resultsDir := filepath.Join(fleetPath, "results")
	if err := os.MkdirAll(resultsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create results directory: %w", err)
	}

	// Create fleet entry
	entry := FleetEntry{
		ID:         fleetID,
		Config:     filepath.Base(configPath),
		Task:       task,
		State:      FleetPending,
		CreatedAt:  nowISO(),
		UpdatedAt:  nowISO(),
		ResultsDir: resultsDir,
	}

	// Save to registry
	registry, err := loadFleetRegistry(root)
	if err != nil {
		return nil, err
	}
	registry.Fleets[fleetID] = entry
	if err := saveFleetRegistry(root, registry); err != nil {
		return nil, err
	}

	// Launch Python runner in background
	cmd := exec.Command("python3", "-m", "lablib.fleet_runner",
		"--fleet-id", fleetID,
		"--fleet-dir", fleetPath,
	)
	cmd.Dir = filepath.Join(root, "projects/cog_lab_package")

	// Redirect output to log file
	logFile, err := os.Create(filepath.Join(fleetPath, "runner.log"))
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		entry.State = FleetFailed
		registry.Fleets[fleetID] = entry
		saveFleetRegistry(root, registry)
		return nil, fmt.Errorf("failed to start runner: %w", err)
	}

	// Update state to running
	entry.State = FleetRunning
	entry.PID = cmd.Process.Pid
	entry.UpdatedAt = nowISO()
	registry.Fleets[fleetID] = entry
	saveFleetRegistry(root, registry)

	return &entry, nil
}

// getFleetStatus gets status of all or specific fleet
func getFleetStatus(root string, fleetID string) (interface{}, error) {
	registry, err := loadFleetRegistry(root)
	if err != nil {
		return nil, err
	}

	if fleetID != "" {
		entry, ok := registry.Fleets[fleetID]
		if !ok {
			return nil, fmt.Errorf("fleet not found: %s", fleetID)
		}

		// Check if process is still running
		if entry.PID > 0 && entry.State == FleetRunning {
			process, err := os.FindProcess(entry.PID)
			if err != nil || process == nil {
				entry.State = FleetFailed
				entry.UpdatedAt = nowISO()
				registry.Fleets[fleetID] = entry
				saveFleetRegistry(root, registry)
			}
		}

		// Try to load results summary
		resultsFile := filepath.Join(root, fleetDir, fleetID, "results", "summary.json")
		if data, err := os.ReadFile(resultsFile); err == nil {
			var summary map[string]interface{}
			if json.Unmarshal(data, &summary) == nil {
				return map[string]interface{}{
					"entry":   entry,
					"summary": summary,
				}, nil
			}
		}

		return entry, nil
	}

	// Return all fleets
	return registry.Fleets, nil
}

// reapFleet collects results and cleans up a completed fleet
func reapFleet(root, fleetID string) (interface{}, error) {
	registry, err := loadFleetRegistry(root)
	if err != nil {
		return nil, err
	}

	entry, ok := registry.Fleets[fleetID]
	if !ok {
		return nil, fmt.Errorf("fleet not found: %s", fleetID)
	}

	// Load results
	resultsFile := filepath.Join(root, fleetDir, fleetID, "results", "summary.json")
	var results map[string]interface{}

	if data, err := os.ReadFile(resultsFile); err == nil {
		if err := json.Unmarshal(data, &results); err != nil {
			results = map[string]interface{}{"error": "failed to parse results"}
		}
	} else {
		results = map[string]interface{}{"error": "no results file found"}
	}

	// Mark as reaped (completed)
	entry.State = FleetCompleted
	entry.UpdatedAt = nowISO()
	registry.Fleets[fleetID] = entry
	saveFleetRegistry(root, registry)

	return map[string]interface{}{
		"fleet_id": fleetID,
		"state":    entry.State,
		"results":  results,
	}, nil
}

// getFleetLogs returns logs from a fleet
func getFleetLogs(root, fleetID string, tail int) (string, error) {
	logFile := filepath.Join(root, fleetDir, fleetID, "runner.log")

	data, err := os.ReadFile(logFile)
	if err != nil {
		return "", fmt.Errorf("failed to read log file: %w", err)
	}

	logs := string(data)

	// Tail if requested
	if tail > 0 {
		lines := strings.Split(logs, "\n")
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		logs = strings.Join(lines, "\n")
	}

	return logs, nil
}

// listFleetConfigs lists available fleet configurations
func listFleetConfigs(root string) ([]string, error) {
	configDir := filepath.Join(root, fleetConfigsDir)

	entries, err := os.ReadDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read configs directory: %w", err)
	}

	var configs []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".yaml") {
			configs = append(configs, strings.TrimSuffix(entry.Name(), ".yaml"))
		}
	}

	return configs, nil
}

// === COMMAND HANDLER ===

func cmdFleet(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found (run from workspace or use -w flag)\n")
		return 1
	}

	if len(args) < 1 {
		printFleetHelp()
		return 0
	}

	subCmd := args[0]

	switch subCmd {
	case "spawn":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog fleet spawn <config> --task \"...\"\n")
			return 1
		}

		configName := args[1]
		task := ""

		// Parse --task flag
		for i := 2; i < len(args); i++ {
			if args[i] == "--task" && i+1 < len(args) {
				task = args[i+1]
				break
			}
		}

		if task == "" {
			fmt.Fprintf(os.Stderr, "Error: --task is required\n")
			return 1
		}

		entry, err := spawnFleet(root, configName, task)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error spawning fleet: %v\n", err)
			return 1
		}

		fmt.Printf("Fleet spawned: %s\n", entry.ID)
		fmt.Printf("  Config: %s\n", entry.Config)
		fmt.Printf("  Task: %s\n", truncate(entry.Task, 60))
		fmt.Printf("  Status: %s\n", entry.State)
		fmt.Printf("\nUse 'cog fleet status %s' to check progress\n", entry.ID)
		return 0

	case "status":
		fleetID := ""
		if len(args) > 1 {
			fleetID = args[1]
		}

		status, err := getFleetStatus(root, fleetID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		if fleetID != "" {
			// Single fleet
			output, _ := json.MarshalIndent(status, "", "  ")
			fmt.Println(string(output))
		} else {
			// All fleets - table format
			fleets, ok := status.(map[string]FleetEntry)
			if !ok || len(fleets) == 0 {
				fmt.Println("No fleets registered")
				return 0
			}

			fmt.Printf("%-16s %-20s %-12s %-10s %s\n",
				"FLEET_ID", "CONFIG", "STATE", "AGE", "TASK")
			fmt.Println(strings.Repeat("-", 80))

			for id, entry := range fleets {
				age := formatAge(entry.CreatedAt)
				task := truncate(entry.Task, 30)
				fmt.Printf("%-16s %-20s %-12s %-10s %s\n",
					id, entry.Config, entry.State, age, task)
			}
		}
		return 0

	case "reap":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog fleet reap <fleet_id>\n")
			return 1
		}

		fleetID := args[1]
		result, err := reapFleet(root, fleetID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))
		return 0

	case "logs":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog fleet logs <fleet_id> [--tail N]\n")
			return 1
		}

		fleetID := args[1]
		tail := 50 // Default tail

		// Parse --tail flag
		for i := 2; i < len(args); i++ {
			if args[i] == "--tail" && i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &tail)
				break
			}
		}

		logs, err := getFleetLogs(root, fleetID, tail)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		fmt.Print(logs)
		return 0

	case "configs", "list":
		configs, err := listFleetConfigs(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		fmt.Println("Available fleet configurations:")
		for _, cfg := range configs {
			fmt.Printf("  - %s\n", cfg)
		}
		return 0

	case "stats":
		// Parse flags
		refresh := false
		for i := 1; i < len(args); i++ {
			if args[i] == "--refresh" || args[i] == "-r" {
				refresh = true
			}
		}

		// Run Python stats script
		scriptPath := filepath.Join(root, "projects/cog_lab_package/lablib/model_stats.py")
		cmdArgs := []string{scriptPath}
		if refresh {
			cmdArgs = append(cmdArgs, "--refresh")
		}

		cmd := exec.Command("python3", cmdArgs...)
		cmd.Dir = filepath.Join(root, "projects/cog_lab_package")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error running stats: %v\n", err)
			return 1
		}
		return 0

	case "help":
		printFleetHelp()
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown fleet command: %s\n", subCmd)
		printFleetHelp()
		return 1
	}
}

func printFleetHelp() {
	fmt.Printf(`Fleet - Agent fleet orchestration (kubectl for cognition)

Usage: cog fleet <command> [args...]

Commands:
  spawn <config> --task "..."   Spawn a fleet from configuration
  status [fleet_id]             Show status of all or specific fleet
  reap <fleet_id>               Collect results from completed fleet
  logs <fleet_id> [--tail N]    View fleet logs
  configs                       List available fleet configurations
  stats [--refresh]             Show OpenRouter model inventory stats

Examples:
  cog fleet spawn research_team --task "Analyze the HD001 objection"
  cog fleet status
  cog fleet status fleet_abc123
  cog fleet reap fleet_abc123
  cog fleet logs fleet_abc123 --tail 100
  cog fleet stats               # Show cached model stats
  cog fleet stats --refresh     # Fetch fresh data from OpenRouter

Fleet Configurations:
  Located in: projects/cog_lab_package/agents/fleet_configs/

  Each config defines:
  - models: Which models to use (kimi-k2, gpt-4o-mini, etc.)
  - roles: analyst, critic, synthesizer, etc.
  - integration_mode: parallel, sequential, dialectic, collaborative

Model Inventory:
  Cached at: projects/cog_lab_package/.cache/openrouter_models.json
  Documentation: projects/cog_lab_package/agents/MODEL_INDEX.md

Architecture:
  Go kernel handles orchestration, Python handles inference.
  Results stored in .cog/fleet/<fleet_id>/results/
`)
}

// === UTILITIES ===

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func formatAge(isoTime string) string {
	t, err := time.Parse(time.RFC3339, isoTime)
	if err != nil {
		return "unknown"
	}

	age := time.Since(t)

	if age < time.Minute {
		return fmt.Sprintf("%ds", int(age.Seconds()))
	} else if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	} else if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}
