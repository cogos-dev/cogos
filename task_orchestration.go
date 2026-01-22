package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// === TASK ORCHESTRATION FUNCTIONS ===

// loadKernelConfig loads and parses the kernel configuration file
func loadKernelConfig(configPath string) (*KernelConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config KernelConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Set task names from map keys
	for name, task := range config.Tasks {
		task.Name = name
		config.Tasks[name] = task
	}

	return &config, nil
}

// buildTaskGraph constructs a dependency graph from task definitions
// skipValidation allows building partial graphs (useful for filtering later)
func buildTaskGraph(tasks map[string]Task) (*TaskGraph, error) {
	graph := &TaskGraph{
		Nodes:    make(map[string]*Task),
		Edges:    make(map[string][]string),
		InDegree: make(map[string]int),
	}

	// Add all tasks as nodes
	for name, task := range tasks {
		t := task // Create copy
		graph.Nodes[name] = &t
		graph.InDegree[name] = 0
	}

	// Build edges from dependencies
	// Edge semantics: task -> dependency (task depends on dependency)
	// In-degree: number of tasks that depend on this task
	for name, task := range tasks {
		graph.Edges[name] = make([]string, 0)
		for _, dep := range task.DependsOn {
			// Skip Turborepo-specific syntax like ^build
			if strings.HasPrefix(dep, "^") {
				continue // Skip upstream dependency markers for now
			}
			// Skip dependencies that don't exist (might be in other workspaces)
			if _, exists := graph.Nodes[dep]; !exists {
				continue // Lenient validation - skip missing deps
			}
			graph.Edges[name] = append(graph.Edges[name], dep)
			// Increment in-degree for the task that has the dependency, not the dependency itself
			graph.InDegree[name]++
		}
	}

	return graph, nil
}

// detectCycles uses DFS to detect cycles in the task graph
func detectCycles(graph *TaskGraph) error {
	// Track visit state: 0 = unvisited, 1 = visiting, 2 = visited
	state := make(map[string]int)
	path := []string{}

	var visit func(string) error
	visit = func(node string) error {
		if state[node] == 1 {
			// Currently visiting - found a cycle
			cycleStart := 0
			for i, n := range path {
				if n == node {
					cycleStart = i
					break
				}
			}
			cycle := append(path[cycleStart:], node)
			return fmt.Errorf("cycle detected: %s", strings.Join(cycle, " -> "))
		}
		if state[node] == 2 {
			// Already visited
			return nil
		}

		state[node] = 1
		path = append(path, node)

		// Visit dependencies
		for _, dep := range graph.Edges[node] {
			if err := visit(dep); err != nil {
				return err
			}
		}

		state[node] = 2
		path = path[:len(path)-1]
		return nil
	}

	// Check all nodes
	for node := range graph.Nodes {
		if state[node] == 0 {
			if err := visit(node); err != nil {
				return err
			}
		}
	}

	return nil
}

// topoSort performs topological sort using Kahn's algorithm
// Returns levels where tasks in the same level can run in parallel
func topoSort(graph *TaskGraph) ([][]string, error) {
	// Create working copy of in-degrees
	inDegree := make(map[string]int)
	for name, deg := range graph.InDegree {
		inDegree[name] = deg
	}

	// Build reverse edges (node -> dependents)
	reverseEdges := make(map[string][]string)
	for node, deps := range graph.Edges {
		for _, dep := range deps {
			reverseEdges[dep] = append(reverseEdges[dep], node)
		}
	}

	levels := [][]string{}
	processed := 0

	for processed < len(graph.Nodes) {
		// Find all nodes with in-degree 0 (can run now)
		currentLevel := []string{}
		for node := range graph.Nodes {
			if inDegree[node] == 0 {
				currentLevel = append(currentLevel, node)
			}
		}

		if len(currentLevel) == 0 {
			// No nodes available but we haven't processed all - cycle detected
			return nil, fmt.Errorf("cycle detected in task graph")
		}

		// Sort level for deterministic execution order
		sort.Strings(currentLevel)
		levels = append(levels, currentLevel)

		// Remove current level nodes and update in-degrees
		for _, node := range currentLevel {
			inDegree[node] = -1 // Mark as processed
			processed++

			// Decrease in-degree for all dependents
			for _, dependent := range reverseEdges[node] {
				inDegree[dependent]--
			}
		}
	}

	return levels, nil
}

// executeTask runs a single task command
func executeTask(task *Task, config *KernelConfig) error {
	// Parse command into program and args
	parts := strings.Fields(task.Command)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Set environment
	cmd.Env = os.Environ()

	// Add global env vars
	for _, env := range config.GlobalEnv {
		if val := os.Getenv(env); val != "" {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", env, val))
		}
	}

	// Add task-specific env vars
	for _, env := range task.Env {
		if val := os.Getenv(env); val != "" {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", env, val))
		}
	}

	return cmd.Run()
}

// runTaskLevel executes all tasks in a level in parallel
func runTaskLevel(taskNames []string, config *KernelConfig) error {
	if len(taskNames) == 0 {
		return nil
	}

	// Use channels for coordination
	type result struct {
		name     string
		err      error
		duration time.Duration
	}

	results := make(chan result, len(taskNames))

	// Launch all tasks in parallel
	for _, name := range taskNames {
		go func(taskName string) {
			task := config.Tasks[taskName]
			start := time.Now()

			fmt.Printf(">>> %s: executing...\n", taskName)
			err := executeTask(&task, config)

			results <- result{
				name:     taskName,
				err:      err,
				duration: time.Since(start),
			}
		}(name)
	}

	// Collect results
	var firstError error
	for i := 0; i < len(taskNames); i++ {
		res := <-results
		if res.err != nil {
			fmt.Printf(">>> %s: failed (%.2fs)\n", res.name, res.duration.Seconds())
			if firstError == nil {
				firstError = res.err
			}
		} else {
			fmt.Printf(">>> %s: completed (%.2fs)\n", res.name, res.duration.Seconds())
		}
	}

	return firstError
}

// runTask executes a task and all its dependencies
func runTask(taskName string, config *KernelConfig) error {
	// Build dependency graph
	graph, err := buildTaskGraph(config.Tasks)
	if err != nil {
		return fmt.Errorf("failed to build task graph: %w", err)
	}

	// Detect cycles
	if err := detectCycles(graph); err != nil {
		return fmt.Errorf("dependency graph error: %w", err)
	}

	// Get all reachable tasks from target
	reachable := getReachableTasks(taskName, graph)

	// Build subgraph with only reachable tasks
	subgraph := &TaskGraph{
		Nodes:    make(map[string]*Task),
		Edges:    make(map[string][]string),
		InDegree: make(map[string]int),
	}

	for _, name := range reachable {
		task := graph.Nodes[name]
		subgraph.Nodes[name] = task
		subgraph.Edges[name] = []string{}
		subgraph.InDegree[name] = 0
	}

	// Rebuild edges for subgraph
	for name := range subgraph.Nodes {
		for _, dep := range graph.Edges[name] {
			if _, exists := subgraph.Nodes[dep]; exists {
				subgraph.Edges[name] = append(subgraph.Edges[name], dep)
				subgraph.InDegree[name]++
			}
		}
	}

	// Topological sort into execution levels
	levels, err := topoSort(subgraph)
	if err != nil {
		return fmt.Errorf("failed to sort tasks: %w", err)
	}

	fmt.Printf("Executing task: %s\n", taskName)
	fmt.Printf("Dependency graph: %d levels, %d total tasks\n", len(levels), len(reachable))

	// Execute levels in order
	for i, level := range levels {
		fmt.Printf("\n--- Level %d: %s ---\n", i+1, strings.Join(level, ", "))
		if err := runTaskLevel(level, config); err != nil {
			return fmt.Errorf("level %d failed: %w", i+1, err)
		}
	}

	fmt.Printf("\n✓ Task %s completed successfully\n", taskName)
	return nil
}

// getReachableTasks performs BFS to find all tasks reachable from start
func getReachableTasks(start string, graph *TaskGraph) []string {
	visited := make(map[string]bool)
	queue := []string{start}
	visited[start] = true
	result := []string{start}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		// Add all dependencies
		for _, dep := range graph.Edges[current] {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
				result = append(result, dep)
			}
		}
	}

	return result
}

// cmdRun executes a task with dependency resolution
func cmdRun(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: cog run <task-name>")
		return 1
	}

	taskName := args[0]

	// Try loading from .cog/cog.yaml first, then fall back to turbo.json
	configPath := ".cog/cog.yaml"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Fall back to turbo.json for compatibility
		configPath = "turbo.json"
	}

	config, err := loadKernelConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	// Verify task exists
	if _, exists := config.Tasks[taskName]; !exists {
		fmt.Fprintf(os.Stderr, "Task not found: %s\n", taskName)
		fmt.Fprintf(os.Stderr, "Available tasks: %s\n", strings.Join(getTaskNames(config.Tasks), ", "))
		return 1
	}

	// Execute task
	if err := runTask(taskName, config); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	return 0
}

// getTaskNames returns sorted list of task names
func getTaskNames(tasks map[string]Task) []string {
	names := make([]string, 0, len(tasks))
	for name := range tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveDeps returns dependency chain as levels for display
func resolveDeps(graph *TaskGraph, taskName string) [][]string {
	reachable := getReachableTasks(taskName, graph)

	// Build subgraph with only reachable tasks
	subgraph := &TaskGraph{
		Nodes:    make(map[string]*Task),
		Edges:    make(map[string][]string),
		InDegree: make(map[string]int),
	}

	for _, name := range reachable {
		task := graph.Nodes[name]
		subgraph.Nodes[name] = task
		subgraph.Edges[name] = []string{}
		subgraph.InDegree[name] = 0
	}

	// Rebuild edges for subgraph
	for name := range subgraph.Nodes {
		for _, dep := range graph.Edges[name] {
			if _, exists := subgraph.Nodes[dep]; exists {
				subgraph.Edges[name] = append(subgraph.Edges[name], dep)
				subgraph.InDegree[name]++
			}
		}
	}

	// Topological sort into levels
	levels, err := topoSort(subgraph)
	if err != nil {
		return [][]string{{taskName}} // Fallback to single level
	}

	return levels
}

// cmdTasks handles task management commands
func cmdTasks(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: cog tasks [list|graph|show] [task-name]")
		return 1
	}

	subcommand := args[0]

	// Load config
	configPath := ".cog/cog.yaml"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = "turbo.json"
	}

	config, err := loadKernelConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	switch subcommand {
	case "list":
		return cmdTasksList(config)
	case "graph":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: cog tasks graph <task-name>")
			return 1
		}
		taskName := args[1]
		return cmdTasksGraph(config, taskName)
	case "show":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: cog tasks show <task-name>")
			return 1
		}
		taskName := args[1]
		return cmdTasksShow(config, taskName)
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subcommand)
		fmt.Fprintln(os.Stderr, "Usage: cog tasks [list|graph|show] [task-name]")
		return 1
	}
}

func cmdTasksList(config *KernelConfig) int {
	fmt.Println("Available tasks:")

	// Sort task names alphabetically
	names := getTaskNames(config.Tasks)

	for _, name := range names {
		task := config.Tasks[name]
		cacheStr := ""
		if task.Cache {
			cacheStr = " [cached]"
		}
		fmt.Printf("  %s%s\n", name, cacheStr)
		if task.Command != "" {
			fmt.Printf("    Command: %s\n", task.Command)
		}
		if len(task.DependsOn) > 0 {
			fmt.Printf("    Depends on: %v\n", task.DependsOn)
		}
	}

	return 0
}

func cmdTasksGraph(config *KernelConfig, taskName string) int {
	// Check task exists
	if _, exists := config.Tasks[taskName]; !exists {
		fmt.Fprintf(os.Stderr, "Task not found: %s\n", taskName)
		return 1
	}

	// Build graph
	graph, err := buildTaskGraph(config.Tasks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building graph: %v\n", err)
		return 1
	}

	// Get dependency chain
	deps := resolveDeps(graph, taskName)

	fmt.Printf("Dependency graph for: %s\n", taskName)
	for i, level := range deps {
		fmt.Printf("Level %d: %v\n", i+1, level)
	}

	return 0
}

func cmdTasksShow(config *KernelConfig, taskName string) int {
	task, exists := config.Tasks[taskName]
	if !exists {
		fmt.Fprintf(os.Stderr, "Task not found: %s\n", taskName)
		return 1
	}

	fmt.Printf("Task: %s\n", taskName)
	fmt.Printf("  Command: %s\n", task.Command)
	fmt.Printf("  Cache: %v\n", task.Cache)
	if len(task.DependsOn) > 0 {
		fmt.Printf("  Dependencies: %v\n", task.DependsOn)
	}
	if len(task.Inputs) > 0 {
		fmt.Printf("  Inputs: %v\n", task.Inputs)
	}
	if len(task.Outputs) > 0 {
		fmt.Printf("  Outputs: %v\n", task.Outputs)
	}
	if len(task.Env) > 0 {
		fmt.Printf("  Environment: %v\n", task.Env)
	}

	return 0
}

// cmdCache handles cache management commands
func cmdCache(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: cog cache [stats|list|clean]")
		return 1
	}

	subcommand := args[0]
	cacheDir := ".cog/.cache"

	switch subcommand {
	case "stats":
		return cmdCacheStats(cacheDir)
	case "list":
		return cmdCacheList(cacheDir)
	case "clean":
		return cmdCacheClean(cacheDir)
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subcommand)
		fmt.Fprintln(os.Stderr, "Usage: cog cache [stats|list|clean]")
		return 1
	}
}

func cmdCacheStats(cacheDir string) int {
	// Count cache entries
	pattern := filepath.Join(cacheDir, "*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading cache: %v\n", err)
		return 1
	}

	totalSize := int64(0)
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		totalSize += info.Size()
	}

	fmt.Printf("Cache statistics:\n")
	fmt.Printf("  Entries: %d\n", len(files))
	fmt.Printf("  Size: %.2f MB\n", float64(totalSize)/(1024*1024))
	fmt.Printf("  Location: %s\n", cacheDir)

	return 0
}

func cmdCacheList(cacheDir string) int {
	pattern := filepath.Join(cacheDir, "*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading cache: %v\n", err)
		return 1
	}

	if len(files) == 0 {
		fmt.Println("No cached tasks")
		return 0
	}

	fmt.Println("Cached tasks:")
	for _, file := range files {
		// Load entry to get task info
		key := filepath.Base(file)
		key = strings.TrimSuffix(key, ".json")

		info, err := os.Stat(file)
		if err != nil {
			continue
		}

		// Truncate hash for display
		displayKey := key
		if len(key) > 16 {
			displayKey = key[:16] + "..."
		}

		fmt.Printf("  %s (%s)\n", displayKey, info.ModTime().Format("2006-01-02 15:04"))
	}

	return 0
}

func cmdCacheClean(cacheDir string) int {
	err := os.RemoveAll(cacheDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error cleaning cache: %v\n", err)
		return 1
	}

	fmt.Println("Cache cleaned")
	return 0
}
