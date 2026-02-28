// sentinel_e2e_test.go
// End-to-end tests validating the Sentinel agent's cron-to-event-stream pipeline.
// Covers: CRD loading, cron projection, event formatting, capability advertisement,
// tool restrictions, event bridge formatting, and job ID stability.

package main

import (
	"strings"
	"testing"
	"time"
)

// workspaceRoot is the real workspace root used for loading agent CRDs.
const workspaceRoot = "/Users/slowbro/cog-workspace"

// ─── Test 1: CRD loads correctly ────────────────────────────────────────────────

func TestSentinelCRDLoadsCorrectly(t *testing.T) {
	crd, err := LoadAgentCRD(workspaceRoot, "sentinel")
	if err != nil {
		t.Fatalf("LoadAgentCRD failed: %v", err)
	}

	// Verify metadata name
	if crd.Metadata.Name != "sentinel" {
		t.Errorf("Metadata.Name = %q, want %q", crd.Metadata.Name, "sentinel")
	}

	// Verify it has cron entries
	if len(crd.Spec.Scheduling.Cron) == 0 {
		t.Fatal("expected at least one cron entry in sentinel CRD, got 0")
	}

	// Verify it has tool restrictions — deny list includes exec, write, edit
	deny := crd.Spec.Capabilities.Tools.Deny
	if len(deny) == 0 {
		t.Fatal("expected non-empty deny list in sentinel CRD")
	}

	requiredDeny := []string{"exec", "write", "edit"}
	for _, tool := range requiredDeny {
		if !sentinelSliceContains(deny, tool) {
			t.Errorf("deny list %v does not contain %q", deny, tool)
		}
	}
}

// ─── Test 2: Cron projection ────────────────────────────────────────────────────

func TestSentinelCronProjection(t *testing.T) {
	crd, err := LoadAgentCRD(workspaceRoot, "sentinel")
	if err != nil {
		t.Fatalf("LoadAgentCRD failed: %v", err)
	}

	// Extract cron entries from CRD
	cronEntries := crd.Spec.Scheduling.Cron
	if len(cronEntries) == 0 {
		t.Fatal("sentinel CRD has no cron entries")
	}

	// Project CRD cron entries into CronJobs
	jobs := projectCRDCronEntries(*crd)
	if len(jobs) == 0 {
		t.Fatal("projectCRDCronEntries returned no jobs")
	}

	if len(jobs) != len(cronEntries) {
		t.Fatalf("projected %d jobs, want %d (one per cron entry)", len(jobs), len(cronEntries))
	}

	expectedAgentID := crdToOpenClawID("sentinel")

	for i, job := range jobs {
		entry := cronEntries[i]

		// Schedule matches CRD
		if job.Schedule.Expr != entry.Schedule {
			t.Errorf("job[%d].Schedule.Expr = %q, want %q", i, job.Schedule.Expr, entry.Schedule)
		}

		// AgentID matches sentinel
		if job.AgentID != expectedAgentID {
			t.Errorf("job[%d].AgentID = %q, want %q", i, job.AgentID, expectedAgentID)
		}

		// Action (payload message) is populated
		if job.Payload.Message == "" {
			t.Errorf("job[%d].Payload.Message is empty", i)
		}
		if job.Payload.Message != entry.Task {
			t.Errorf("job[%d].Payload.Message = %q, want %q", i, job.Payload.Message, entry.Task)
		}

		// Schedule kind should be "cron"
		if job.Schedule.Kind != "cron" {
			t.Errorf("job[%d].Schedule.Kind = %q, want %q", i, job.Schedule.Kind, "cron")
		}

		// JobID should be non-empty
		if job.JobID == "" {
			t.Errorf("job[%d].JobID is empty", i)
		}

		// ManagedBy should be "cogos"
		if job.ManagedBy != "cogos" {
			t.Errorf("job[%d].ManagedBy = %q, want %q", i, job.ManagedBy, "cogos")
		}
	}

	// Verify stable job ID generation: same inputs produce same hash
	jobs2 := projectCRDCronEntries(*crd)
	for i := range jobs {
		if jobs[i].JobID != jobs2[i].JobID {
			t.Errorf("job[%d] ID not stable: %q != %q", i, jobs[i].JobID, jobs2[i].JobID)
		}
	}
}

// ─── Test 3: Event formatting ───────────────────────────────────────────────────

func TestSentinelEventFormatting(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	tests := []struct {
		name     string
		event    BusEventData
		wantSub  []string // substrings that must appear in output
		wantType string   // the event type label expected in output
	}{
		{
			name: "chat.request",
			event: BusEventData{
				Type: "chat.request",
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"origin":  "sentinel",
					"content": "Checking event streams for anomalies",
				},
			},
			wantSub:  []string{"sentinel", "chat.request"},
			wantType: "chat.request",
		},
		{
			name: "chat.response",
			event: BusEventData{
				Type: "chat.response",
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"tokens_used": float64(150),
					"duration_ms": float64(2500),
				},
			},
			wantSub:  []string{"sentinel", "chat.response", "150 tokens", "2500ms"},
			wantType: "chat.response",
		},
		{
			name: "chat.error",
			event: BusEventData{
				Type: "chat.error",
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"error": "rate limit exceeded",
				},
			},
			wantSub:  []string{"sentinel", "chat.error", "rate limit exceeded"},
			wantType: "chat.error",
		},
		{
			name: "tool.invoke",
			event: BusEventData{
				Type: BlockToolInvoke,
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"callerAgent": "sentinel",
					"targetAgent": "cog",
					"tool":        "memory_search",
				},
			},
			wantSub:  []string{"sentinel", "tool.invoke", "memory_search"},
			wantType: "tool.invoke",
		},
		{
			name: "tool.result",
			event: BusEventData{
				Type: BlockToolResult,
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"executedBy": "sentinel",
					"tool":       "memory_search",
					"durationMs": float64(120),
				},
			},
			wantSub:  []string{"sentinel", "tool.result", "memory_search", "120ms"},
			wantType: "tool.result",
		},
		{
			name: "agent.capabilities",
			event: BusEventData{
				Type: BlockAgentCapabilities,
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"agentId": "sentinel",
					"tools": map[string]interface{}{
						"allow": []interface{}{"read", "web_fetch", "web_search"},
					},
				},
			},
			wantSub:  []string{"sentinel", "agent.capabilities", "3 tools"},
			wantType: "agent.capabilities",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatBusEvent(&tt.event)
			if result == "" {
				t.Fatalf("formatBusEvent returned empty string for %s", tt.name)
			}

			for _, sub := range tt.wantSub {
				if !strings.Contains(result, sub) {
					t.Errorf("output %q does not contain %q", result, sub)
				}
			}
		})
	}

	// Verify timestamp extraction works (RFC3339 -> HH:MM:SS)
	ts := "2026-02-24T14:30:45Z"
	formatted := formatEventTimestamp(ts)
	if formatted != "14:30:45" {
		t.Errorf("formatEventTimestamp(%q) = %q, want %q", ts, formatted, "14:30:45")
	}

	// Verify RFC3339Nano also works
	tsNano := "2026-02-24T14:30:45.123456789Z"
	formattedNano := formatEventTimestamp(tsNano)
	if formattedNano != "14:30:45" {
		t.Errorf("formatEventTimestamp(%q) = %q, want %q", tsNano, formattedNano, "14:30:45")
	}
}

// ─── Test 4: Capability advertisement ───────────────────────────────────────────

func TestSentinelCapabilityAdvertisement(t *testing.T) {
	crd, err := LoadAgentCRD(workspaceRoot, "sentinel")
	if err != nil {
		t.Fatalf("LoadAgentCRD failed: %v", err)
	}

	payload := BuildCapabilitiesPayload(*crd)

	// AgentID == "sentinel"
	if payload.AgentID != "sentinel" {
		t.Errorf("AgentID = %q, want %q", payload.AgentID, "sentinel")
	}

	// Allowed tools match CRD spec
	crdAllow := crd.Spec.Capabilities.Tools.Allow
	if len(payload.Tools.Allow) != len(crdAllow) {
		t.Errorf("Tools.Allow length = %d, want %d", len(payload.Tools.Allow), len(crdAllow))
	}
	for _, tool := range crdAllow {
		if !sentinelSliceContains(payload.Tools.Allow, tool) {
			t.Errorf("Tools.Allow missing %q", tool)
		}
	}

	// Deny tools are populated
	crdDeny := crd.Spec.Capabilities.Tools.Deny
	if len(payload.Tools.Deny) == 0 {
		t.Fatal("Tools.Deny is empty, expected sentinel deny list")
	}
	if len(payload.Tools.Deny) != len(crdDeny) {
		t.Errorf("Tools.Deny length = %d, want %d", len(payload.Tools.Deny), len(crdDeny))
	}
	for _, tool := range crdDeny {
		if !sentinelSliceContains(payload.Tools.Deny, tool) {
			t.Errorf("Tools.Deny missing %q", tool)
		}
	}

	// Advertise flag matches CRD
	if !crd.Spec.Capabilities.Advertise {
		t.Error("CRD spec.capabilities.advertise = false, expected true for sentinel")
	}

	// AgentType matches CRD spec.type
	if payload.AgentType != crd.Spec.Type {
		t.Errorf("AgentType = %q, want %q", payload.AgentType, crd.Spec.Type)
	}

	// Endpoint matches CRD spec.bus.endpoint
	if payload.Endpoint != crd.Spec.Bus.Endpoint {
		t.Errorf("Endpoint = %q, want %q", payload.Endpoint, crd.Spec.Bus.Endpoint)
	}

	// AdvertisedAt should be recent
	if time.Since(payload.AdvertisedAt) > 2*time.Second {
		t.Errorf("AdvertisedAt too old: %v", payload.AdvertisedAt)
	}
}

// ─── Test 5: Tool restrictions ──────────────────────────────────────────────────

func TestSentinelToolRestrictions(t *testing.T) {
	policy, err := GetAgentCRDToolPolicy(workspaceRoot, "sentinel")
	if err != nil {
		t.Fatalf("GetAgentCRDToolPolicy failed: %v", err)
	}
	if policy == nil {
		t.Fatal("GetAgentCRDToolPolicy returned nil policy for sentinel")
	}

	// Verify denied tools are in deny list
	expectedDeny := []string{"exec", "write", "edit", "process", "browser"}
	for _, tool := range expectedDeny {
		if !sentinelSliceContains(policy.DenyTools, tool) {
			t.Errorf("denied tool %q not found in policy.DenyTools %v", tool, policy.DenyTools)
		}
	}

	// Verify allowed tools are in allow list or not in deny list
	expectedAllow := []string{"read", "web_fetch", "web_search", "message", "memory_search", "memory_get"}
	for _, tool := range expectedAllow {
		if sentinelSliceContains(policy.DenyTools, tool) {
			t.Errorf("allowed tool %q found in deny list %v", tool, policy.DenyTools)
		}
	}

	// DangerouslySkipPermissions should be false for sentinel
	if policy.DangerouslySkipPermissions {
		t.Error("DangerouslySkipPermissions = true, want false for sentinel")
	}

	// AllowedTools should reflect the claude-code shell override
	if len(policy.AllowedTools) == 0 {
		t.Error("AllowedTools is empty, expected claude-code allowedTools override")
	}
	claudeCodeAllowed := []string{"Read", "Grep", "Glob", "WebFetch", "WebSearch"}
	for _, tool := range claudeCodeAllowed {
		if !sentinelSliceContains(policy.AllowedTools, tool) {
			t.Errorf("claude-code allowedTool %q missing from policy.AllowedTools %v", tool, policy.AllowedTools)
		}
	}
}

// ─── Test 6: Event bridge formatting ────────────────────────────────────────────

func TestSentinelEventBridgeFormatting(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	eventTypes := []struct {
		name  string
		event BusEventData
	}{
		{
			name: "chat.request",
			event: BusEventData{
				Type:    "chat.request",
				From:    "sentinel",
				Ts:      now,
				Payload: map[string]interface{}{"content": "test message with\nnewlines\nin it"},
			},
		},
		{
			name: "chat.response",
			event: BusEventData{
				Type:    "chat.response",
				From:    "sentinel",
				Ts:      now,
				Payload: map[string]interface{}{"tokens_used": float64(50)},
			},
		},
		{
			name: "chat.error",
			event: BusEventData{
				Type:    "chat.error",
				From:    "sentinel",
				Ts:      now,
				Payload: map[string]interface{}{"error": "something\nwent\nwrong"},
			},
		},
		{
			name: "tool.invoke",
			event: BusEventData{
				Type:    BlockToolInvoke,
				From:    "sentinel",
				Ts:      now,
				Payload: map[string]interface{}{"callerAgent": "sentinel", "tool": "read"},
			},
		},
		{
			name: "tool.result",
			event: BusEventData{
				Type:    BlockToolResult,
				From:    "sentinel",
				Ts:      now,
				Payload: map[string]interface{}{"executedBy": "sentinel", "tool": "read", "durationMs": float64(42)},
			},
		},
		{
			name: "agent.capabilities",
			event: BusEventData{
				Type: BlockAgentCapabilities,
				From: "sentinel",
				Ts:   now,
				Payload: map[string]interface{}{
					"agentId": "sentinel",
					"tools": map[string]interface{}{
						"allow": []interface{}{"read"},
					},
				},
			},
		},
	}

	for _, tt := range eventTypes {
		t.Run(tt.name, func(t *testing.T) {
			result := formatBusEvent(&tt.event)

			// No empty output
			if result == "" {
				t.Fatalf("formatBusEvent returned empty string for %s", tt.name)
			}

			// No empty lines (no \n in single-line output)
			if strings.Contains(result, "\n") {
				t.Errorf("output contains newline: %q", result)
			}

			// Timestamp is present (HH:MM:SS pattern)
			if !strings.Contains(result, "[") || !strings.Contains(result, "]") {
				t.Errorf("output missing timestamp brackets: %q", result)
			}

			// Agent name appears in output
			if !strings.Contains(result, "sentinel") {
				t.Errorf("output does not contain 'sentinel': %q", result)
			}

			// Content is properly truncated and sanitized (no raw newlines from payload)
			// The chat.request case has newlines in content that should be sanitized
			if tt.name == "chat.request" {
				// The content field had newlines; they should be stripped
				if strings.Contains(result, "\n") {
					t.Errorf("newlines from payload content leaked into output: %q", result)
				}
			}
		})
	}
}

// ─── Test 7: Cron job ID stability ──────────────────────────────────────────────

func TestCronJobIDStability(t *testing.T) {
	crd, err := LoadAgentCRD(workspaceRoot, "sentinel")
	if err != nil {
		t.Fatalf("LoadAgentCRD failed: %v", err)
	}

	if len(crd.Spec.Scheduling.Cron) == 0 {
		t.Fatal("sentinel CRD has no cron entries")
	}

	agentID := crdToOpenClawID("sentinel")

	for i, entry := range crd.Spec.Scheduling.Cron {
		// Generate job ID twice
		id1 := cronJobID(agentID, entry.Schedule)
		id2 := cronJobID(agentID, entry.Schedule)

		if id1 != id2 {
			t.Errorf("cron entry[%d] job ID not deterministic: %q != %q", i, id1, id2)
		}

		// Verify the ID starts with the agent ID prefix
		if !strings.HasPrefix(id1, agentID+"-") {
			t.Errorf("cron entry[%d] job ID %q does not start with %q-", i, id1, agentID)
		}

		// Verify the ID is non-empty and has a hex suffix
		parts := strings.SplitN(id1, "-", 2)
		if len(parts) != 2 || parts[1] == "" {
			t.Errorf("cron entry[%d] job ID %q has invalid format (expected agentID-hexhash)", i, id1)
		}
	}

	// Also verify via the full projection path
	jobs1 := projectCRDCronEntries(*crd)
	jobs2 := projectCRDCronEntries(*crd)

	if len(jobs1) != len(jobs2) {
		t.Fatalf("projection count mismatch: %d vs %d", len(jobs1), len(jobs2))
	}

	for i := range jobs1 {
		if jobs1[i].JobID != jobs2[i].JobID {
			t.Errorf("projected job[%d] ID not stable: %q != %q", i, jobs1[i].JobID, jobs2[i].JobID)
		}
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

// sentinelSliceContains checks if a string slice contains a given string.
func sentinelSliceContains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
