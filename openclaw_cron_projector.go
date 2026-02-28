// openclaw_cron_projector.go
// Projects agent CRD scheduling.cron[] entries into OpenClaw's cron job format
// at ~/.openclaw/cron/jobs.json.
//
// Each CRD cron entry becomes a CronJob with a deterministic jobId derived from
// agentID + schedule expression. Manual (non-cogos) jobs in jobs.json are preserved.
//
// Usage:
//   cog reconcile plan openclaw-cron
//   cog reconcile apply openclaw-cron

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func init() {
	RegisterProvider("openclaw-cron", &OpenClawCronProjector{})
}

// OpenClawCronProjector implements Reconcilable for cron job projection.
type OpenClawCronProjector struct {
	cronPath string // resolved path to jobs.json (overridable for testing)
}

func (p *OpenClawCronProjector) Type() string { return "openclaw-cron" }

// ─── OpenClaw Cron Job types ─────────────────────────────────────────────────

// CronJob matches OpenClaw's cron job format.
type CronJob struct {
	JobID         string       `json:"jobId"`
	Name          string       `json:"name"`
	Enabled       bool         `json:"enabled"`
	AgentID       string       `json:"agentId"`
	Schedule      CronSchedule `json:"schedule"`
	SessionTarget string       `json:"sessionTarget"`
	Payload       CronPayload  `json:"payload"`
	Delivery      CronDelivery `json:"delivery"`
	ManagedBy     string       `json:"managedBy,omitempty"`
}

// CronSchedule describes the timing of a cron job.
type CronSchedule struct {
	Kind string `json:"kind"` // "cron"
	Expr string `json:"expr"` // cron expression
	TZ   string `json:"tz"`   // timezone
}

// CronPayload describes what the cron job does when it fires.
type CronPayload struct {
	Kind           string `json:"kind"` // "agentTurn"
	Message        string `json:"message"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// CronDelivery describes how the cron job output is delivered.
type CronDelivery struct {
	Mode    string `json:"mode"` // "announce"
	Channel string `json:"channel"`
}

// ─── Config types (declared state from CRDs) ─────────────────────────────────

// cronDeclaredConfig is the declared state: all cron entries across agent CRDs.
type cronDeclaredConfig struct {
	Jobs []CronJob
}

// ─── Live types (current state in jobs.json) ──────────────────────────────────

// cronLiveState represents the current contents of ~/.openclaw/cron/jobs.json.
type cronLiveState struct {
	Jobs []CronJob
}

// ─── CRD cron entry -> CronJob projection ─────────────────────────────────────

// cronJobID generates a deterministic job ID from agentID and schedule expression.
// Uses a short hash to avoid filesystem-unfriendly characters from cron expressions.
func cronJobID(agentID, schedule string) string {
	h := sha256.Sum256([]byte(agentID + ":" + schedule))
	return fmt.Sprintf("%s-%x", agentID, h[:4])
}

// cronJobName generates a human-readable name for a cron job.
func cronJobName(agentName, task string) string {
	// Truncate task to a reasonable length for the name
	summary := task
	if len(summary) > 60 {
		summary = summary[:57] + "..."
	}
	return fmt.Sprintf("%s: %s", agentName, summary)
}

// projectCRDCronEntries converts all cron entries from a single CRD into CronJobs.
func projectCRDCronEntries(crd AgentCRD) []CronJob {
	if len(crd.Spec.Scheduling.Cron) == 0 {
		return nil
	}

	agentID := crdToOpenClawID(crd.Metadata.Name)
	agentName := crd.Spec.Identity.Name

	var jobs []CronJob
	for _, entry := range crd.Spec.Scheduling.Cron {
		// Determine delivery channel: entry-level override > shell channel > default
		channel := entry.Channel
		if channel == "" {
			if oc := crd.Spec.Runtime.Shells.OpenClaw; oc != nil && oc.Channel != "" {
				channel = oc.Channel
			}
		}

		job := CronJob{
			JobID:         cronJobID(agentID, entry.Schedule),
			Name:          cronJobName(agentName, entry.Task),
			Enabled:       true,
			AgentID:       agentID,
			SessionTarget: "new",
			Schedule: CronSchedule{
				Kind: "cron",
				Expr: entry.Schedule,
				TZ:   "UTC",
			},
			Payload: CronPayload{
				Kind:           "agentTurn",
				Message:        entry.Task,
				TimeoutSeconds: 300,
			},
			Delivery: CronDelivery{
				Mode:    "announce",
				Channel: channel,
			},
			ManagedBy: "cogos",
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// ─── Reconcilable implementation ──────────────────────────────────────────────

func (p *OpenClawCronProjector) LoadConfig(root string) (any, error) {
	crds, err := ListAgentCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("openclaw-cron: load CRDs: %w", err)
	}

	var allJobs []CronJob
	for _, crd := range crds {
		jobs := projectCRDCronEntries(crd)
		allJobs = append(allJobs, jobs...)
	}

	return &cronDeclaredConfig{Jobs: allJobs}, nil
}

func (p *OpenClawCronProjector) FetchLive(ctx context.Context, config any) (any, error) {
	cronPath := p.resolveCronPath()

	data, err := os.ReadFile(cronPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &cronLiveState{}, nil
		}
		return nil, fmt.Errorf("openclaw-cron: read jobs.json: %w", err)
	}

	jobs, err := unmarshalCronJobs(data)
	if err != nil {
		return nil, fmt.Errorf("openclaw-cron: parse jobs.json: %w", err)
	}

	return &cronLiveState{Jobs: jobs}, nil
}

func (p *OpenClawCronProjector) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	cfg := config.(*cronDeclaredConfig)
	liveState := live.(*cronLiveState)

	plan := &ReconcilePlan{
		ResourceType: "openclaw-cron",
		GeneratedAt:  time.Now().Format(time.RFC3339),
	}

	// Index live managed jobs by jobId
	liveManagedByID := make(map[string]CronJob)
	for _, job := range liveState.Jobs {
		if job.ManagedBy == "cogos" {
			liveManagedByID[job.JobID] = job
		}
	}

	// Track which declared job IDs we've seen
	declaredIDs := make(map[string]bool)

	// Check each declared job against live state
	for _, desired := range cfg.Jobs {
		declaredIDs[desired.JobID] = true
		liveJob, exists := liveManagedByID[desired.JobID]

		if !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "openclaw-cron-job",
				Name:         desired.JobID,
				Details: map[string]any{
					"agent_id": desired.AgentID,
					"schedule": desired.Schedule.Expr,
					"task":     desired.Payload.Message,
					"payload":  desired,
				},
			})
			plan.Summary.Creates++
		} else if !cronJobsEqual(desired, liveJob) {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "openclaw-cron-job",
				Name:         desired.JobID,
				Details: map[string]any{
					"agent_id": desired.AgentID,
					"schedule": desired.Schedule.Expr,
					"task":     desired.Payload.Message,
					"payload":  desired,
				},
			})
			plan.Summary.Updates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "openclaw-cron-job",
				Name:         desired.JobID,
				Details: map[string]any{
					"agent_id": desired.AgentID,
					"reason":   "in sync",
				},
			})
			plan.Summary.Skipped++
		}
	}

	// Managed jobs that are no longer declared should be deleted
	for jobID, liveJob := range liveManagedByID {
		if !declaredIDs[jobID] {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "openclaw-cron-job",
				Name:         jobID,
				Details: map[string]any{
					"agent_id": liveJob.AgentID,
					"reason":   "no longer declared in CRD",
				},
			})
			plan.Summary.Deletes++
		}
	}

	// Unmanaged jobs are left untouched — just note them
	for _, job := range liveState.Jobs {
		if job.ManagedBy != "cogos" {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "openclaw-cron-job",
				Name:         job.JobID,
				Details: map[string]any{
					"agent_id": job.AgentID,
					"reason":   "unmanaged — not owned by cogos",
				},
			})
			plan.Summary.Skipped++
		}
	}

	return plan, nil
}

func (p *OpenClawCronProjector) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	cronPath := p.resolveCronPath()

	// Read existing jobs (or start fresh)
	var existingJobs []CronJob
	data, err := os.ReadFile(cronPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("openclaw-cron: read jobs.json for apply: %w", err)
		}
		// File doesn't exist yet — start with empty list
	} else {
		parsed, parseErr := unmarshalCronJobs(data)
		if parseErr != nil {
			return nil, fmt.Errorf("openclaw-cron: parse jobs.json for apply: %w", parseErr)
		}
		existingJobs = parsed
	}

	// Index existing jobs by ID for mutation
	jobsByID := make(map[string]int)
	for i, job := range existingJobs {
		jobsByID[job.JobID] = i
	}

	var results []ReconcileResult
	modified := false

	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			results = append(results, ReconcileResult{
				Action: string(ActionSkip),
				Name:   action.Name,
				Status: ApplySkipped,
			})
			continue
		}

		switch action.Action {
		case ActionCreate:
			payloadRaw, ok := action.Details["payload"]
			if !ok {
				results = append(results, ReconcileResult{
					Action: string(ActionCreate),
					Name:   action.Name,
					Status: ApplyFailed,
					Error:  "missing payload in action details",
				})
				continue
			}
			desired, ok := payloadRaw.(CronJob)
			if !ok {
				results = append(results, ReconcileResult{
					Action: string(ActionCreate),
					Name:   action.Name,
					Status: ApplyFailed,
					Error:  "invalid payload type",
				})
				continue
			}
			existingJobs = append(existingJobs, desired)
			jobsByID[desired.JobID] = len(existingJobs) - 1
			modified = true
			log.Printf("[openclaw-cron] Created job %q (%s)", desired.JobID, desired.Schedule.Expr)
			results = append(results, ReconcileResult{
				Action: string(ActionCreate),
				Name:   desired.JobID,
				Status: ApplySucceeded,
			})

		case ActionUpdate:
			payloadRaw, ok := action.Details["payload"]
			if !ok {
				results = append(results, ReconcileResult{
					Action: string(ActionUpdate),
					Name:   action.Name,
					Status: ApplyFailed,
					Error:  "missing payload in action details",
				})
				continue
			}
			desired, ok := payloadRaw.(CronJob)
			if !ok {
				results = append(results, ReconcileResult{
					Action: string(ActionUpdate),
					Name:   action.Name,
					Status: ApplyFailed,
					Error:  "invalid payload type",
				})
				continue
			}
			if idx, ok := jobsByID[desired.JobID]; ok {
				existingJobs[idx] = desired
				modified = true
				log.Printf("[openclaw-cron] Updated job %q (%s)", desired.JobID, desired.Schedule.Expr)
				results = append(results, ReconcileResult{
					Action: string(ActionUpdate),
					Name:   desired.JobID,
					Status: ApplySucceeded,
				})
			}

		case ActionDelete:
			jobID := action.Name
			if idx, ok := jobsByID[jobID]; ok {
				existingJobs = append(existingJobs[:idx], existingJobs[idx+1:]...)
				// Rebuild index after deletion
				jobsByID = make(map[string]int)
				for i, job := range existingJobs {
					jobsByID[job.JobID] = i
				}
				modified = true
				log.Printf("[openclaw-cron] Deleted job %q", jobID)
				results = append(results, ReconcileResult{
					Action: string(ActionDelete),
					Name:   jobID,
					Status: ApplySucceeded,
				})
			}
		}
	}

	if !modified {
		return results, nil
	}

	// Ensure directory exists
	cronDir := filepath.Dir(cronPath)
	if err := os.MkdirAll(cronDir, 0755); err != nil {
		return results, fmt.Errorf("openclaw-cron: create cron directory: %w", err)
	}

	// Write the updated jobs list
	output, err := json.MarshalIndent(existingJobs, "", "  ")
	if err != nil {
		return results, fmt.Errorf("openclaw-cron: marshal jobs: %w", err)
	}
	output = append(output, '\n')

	tmpPath := cronPath + ".tmp"
	if err := os.WriteFile(tmpPath, output, 0644); err != nil {
		return results, fmt.Errorf("openclaw-cron: write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, cronPath); err != nil {
		os.Remove(tmpPath)
		return results, fmt.Errorf("openclaw-cron: atomic rename: %w", err)
	}

	log.Printf("[openclaw-cron] Wrote %d jobs to %s", len(existingJobs), cronPath)
	return results, nil
}

func (p *OpenClawCronProjector) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState := live.(*cronLiveState)

	state := &ReconcileState{
		Version:      1,
		Serial:       1,
		Lineage:      "openclaw-cron",
		ResourceType: "openclaw-cron",
		GeneratedAt:  time.Now().Format(time.RFC3339),
	}

	for _, job := range liveState.Jobs {
		mode := ModeManaged
		unmanagedReason := ""
		if job.ManagedBy != "cogos" {
			mode = ModeUnmanaged
			unmanagedReason = "not managed by cogos"
		}

		state.Resources = append(state.Resources, ReconcileResource{
			Address:         "cron.jobs." + job.JobID,
			Type:            "openclaw-cron-job",
			Mode:            mode,
			ExternalID:      job.JobID,
			Name:            job.Name,
			ParentAddress:   "agents.list." + job.AgentID,
			ParentID:        job.AgentID,
			UnmanagedReason: unmanagedReason,
			Attributes: map[string]any{
				"schedule":  job.Schedule.Expr,
				"agent_id":  job.AgentID,
				"enabled":   job.Enabled,
				"channel":   job.Delivery.Channel,
				"task":      job.Payload.Message,
				"managedBy": job.ManagedBy,
			},
			LastRefreshed: time.Now().Format(time.RFC3339),
		})
	}

	return state, nil
}

func (p *OpenClawCronProjector) Health() ResourceStatus {
	cronPath := p.resolveCronPath()
	if _, err := os.Stat(cronPath); err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   "jobs.json not found (will be created on first apply)",
		}
	}
	return NewResourceStatus(SyncStatusUnknown, HealthHealthy)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func (p *OpenClawCronProjector) resolveCronPath() string {
	if p.cronPath != "" {
		return p.cronPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "cron", "jobs.json")
}

// unmarshalCronJobs handles both raw array format and OpenClaw's wrapper format:
//   Raw array:  [{"jobId":...}, ...]
//   Wrapper:    {"version":1, "jobs":[{"jobId":...}, ...]}
func unmarshalCronJobs(data []byte) ([]CronJob, error) {
	// Try raw array first (our projector's output format)
	var jobs []CronJob
	if err := json.Unmarshal(data, &jobs); err == nil {
		return jobs, nil
	}

	// Try wrapper format (OpenClaw's native format)
	var wrapper struct {
		Version int       `json:"version"`
		Jobs    []CronJob `json:"jobs"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("unrecognized jobs.json format: %w", err)
	}
	return wrapper.Jobs, nil
}

// cronJobsEqual compares two CronJobs for drift (ignoring ManagedBy which is always "cogos").
func cronJobsEqual(desired, live CronJob) bool {
	return desired.JobID == live.JobID &&
		desired.Name == live.Name &&
		desired.Enabled == live.Enabled &&
		desired.AgentID == live.AgentID &&
		desired.SessionTarget == live.SessionTarget &&
		desired.Schedule.Kind == live.Schedule.Kind &&
		desired.Schedule.Expr == live.Schedule.Expr &&
		desired.Schedule.TZ == live.Schedule.TZ &&
		desired.Payload.Kind == live.Payload.Kind &&
		desired.Payload.Message == live.Payload.Message &&
		desired.Payload.TimeoutSeconds == live.Payload.TimeoutSeconds &&
		desired.Delivery.Mode == live.Delivery.Mode &&
		desired.Delivery.Channel == live.Delivery.Channel
}

// ProjectCronJobs is a convenience function that loads all agent CRDs with
// scheduling.cron entries and writes them to ~/.openclaw/cron/jobs.json
// in OpenClaw's cron format. It preserves manually-created jobs (those
// without managedBy: "cogos").
func ProjectCronJobs(root string, homeDir string) error {
	p := &OpenClawCronProjector{
		cronPath: filepath.Join(homeDir, ".openclaw", "cron", "jobs.json"),
	}

	config, err := p.LoadConfig(root)
	if err != nil {
		return err
	}

	ctx := context.Background()
	live, err := p.FetchLive(ctx, config)
	if err != nil {
		return err
	}

	plan, err := p.ComputePlan(config, live, nil)
	if err != nil {
		return err
	}

	if !plan.Summary.HasChanges() {
		log.Printf("[openclaw-cron] No changes needed (%d jobs in sync)", plan.Summary.Skipped)
		return nil
	}

	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		return err
	}

	for _, r := range results {
		if r.Status == ApplyFailed {
			return fmt.Errorf("openclaw-cron: job %q failed: %s", r.Name, r.Error)
		}
	}

	return nil
}

// slugifyCron is used only in testing/debugging — kept private. For production
// job IDs we use cronJobID which uses a hash for safety.
var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slugifyCron(s string) string {
	slug := nonAlnum.ReplaceAllString(s, "-")
	slug = strings.Trim(slug, "-")
	return slug
}
