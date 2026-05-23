package reconcile_test

import (
	"reflect"
	"testing"

	shim "github.com/myrgic/cogos/pkg/reconcile"
	canonical "github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// TestTypeIdentity verifies that type aliases in the legacy re-export shim
// are identical to their canonical types via reflect.TypeOf. Because Go type
// aliases share the same runtime type, these must always be equal.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name      string
		canonical reflect.Type
		shim      reflect.Type
	}{
		{"SyncStatus", reflect.TypeOf(canonical.SyncStatus("")), reflect.TypeOf(shim.SyncStatus(""))},
		{"HealthStatus", reflect.TypeOf(canonical.HealthStatus("")), reflect.TypeOf(shim.HealthStatus(""))},
		{"OperationPhase", reflect.TypeOf(canonical.OperationPhase("")), reflect.TypeOf(shim.OperationPhase(""))},
		{"ActionType", reflect.TypeOf(canonical.ActionType("")), reflect.TypeOf(shim.ActionType(""))},
		{"ResourceMode", reflect.TypeOf(canonical.ResourceMode("")), reflect.TypeOf(shim.ResourceMode(""))},
		{"ApplyStatus", reflect.TypeOf(canonical.ApplyStatus("")), reflect.TypeOf(shim.ApplyStatus(""))},
		{"AcknowledgmentDecision", reflect.TypeOf(canonical.AcknowledgmentDecision("")), reflect.TypeOf(shim.AcknowledgmentDecision(""))},
		{"ReviewActionTaken", reflect.TypeOf(canonical.ReviewActionTaken("")), reflect.TypeOf(shim.ReviewActionTaken(""))},
		{"ResourceStatus", reflect.TypeOf(canonical.ResourceStatus{}), reflect.TypeOf(shim.ResourceStatus{})},
		{"Plan", reflect.TypeOf(canonical.Plan{}), reflect.TypeOf(shim.Plan{})},
		{"Action", reflect.TypeOf(canonical.Action{}), reflect.TypeOf(shim.Action{})},
		{"Summary", reflect.TypeOf(canonical.Summary{}), reflect.TypeOf(shim.Summary{})},
		{"Result", reflect.TypeOf(canonical.Result{}), reflect.TypeOf(shim.Result{})},
		{"State", reflect.TypeOf(canonical.State{}), reflect.TypeOf(shim.State{})},
		{"Resource", reflect.TypeOf(canonical.Resource{}), reflect.TypeOf(shim.Resource{})},
		{"Event", reflect.TypeOf(canonical.Event{}), reflect.TypeOf(shim.Event{})},
		{"MetaResource", reflect.TypeOf(canonical.MetaResource{}), reflect.TypeOf(shim.MetaResource{})},
		{"MetaConfig", reflect.TypeOf(canonical.MetaConfig{}), reflect.TypeOf(shim.MetaConfig{})},
		{"MetaResult", reflect.TypeOf(canonical.MetaResult{}), reflect.TypeOf(shim.MetaResult{})},
		{"MetaOpts", reflect.TypeOf(canonical.MetaOpts{}), reflect.TypeOf(shim.MetaOpts{})},
		{"CogdocReviewClass", reflect.TypeOf(canonical.CogdocReviewClass{}), reflect.TypeOf(shim.CogdocReviewClass{})},
		{"CogdocProposal", reflect.TypeOf(canonical.CogdocProposal{}), reflect.TypeOf(shim.CogdocProposal{})},
		{"SimilarityCandidate", reflect.TypeOf(canonical.SimilarityCandidate{}), reflect.TypeOf(shim.SimilarityCandidate{})},
		{"CandidateAcknowledgment", reflect.TypeOf(canonical.CandidateAcknowledgment{}), reflect.TypeOf(shim.CandidateAcknowledgment{})},
		{"ProvenanceRecord", reflect.TypeOf(canonical.ProvenanceRecord{}), reflect.TypeOf(shim.ProvenanceRecord{})},
		{"ReviewTRMTuple", reflect.TypeOf(canonical.ReviewTRMTuple{}), reflect.TypeOf(shim.ReviewTRMTuple{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.canonical != tc.shim {
				t.Errorf("%s: canonical type %v != shim type %v (alias is broken)", tc.name, tc.canonical, tc.shim)
			}
		})
	}
}

// TestConstantIdentity verifies that re-exported constants have the same value
// as their canonical counterparts.
func TestConstantIdentity(t *testing.T) {
	// SyncStatus constants
	if canonical.SyncStatusSynced != shim.SyncStatusSynced {
		t.Errorf("SyncStatusSynced mismatch")
	}
	if canonical.SyncStatusOutOfSync != shim.SyncStatusOutOfSync {
		t.Errorf("SyncStatusOutOfSync mismatch")
	}
	if canonical.SyncStatusUnknown != shim.SyncStatusUnknown {
		t.Errorf("SyncStatusUnknown mismatch")
	}

	// HealthStatus constants
	if canonical.HealthHealthy != shim.HealthHealthy {
		t.Errorf("HealthHealthy mismatch")
	}
	if canonical.HealthDegraded != shim.HealthDegraded {
		t.Errorf("HealthDegraded mismatch")
	}
	if canonical.HealthProgressing != shim.HealthProgressing {
		t.Errorf("HealthProgressing mismatch")
	}
	if canonical.HealthMissing != shim.HealthMissing {
		t.Errorf("HealthMissing mismatch")
	}
	if canonical.HealthSuspended != shim.HealthSuspended {
		t.Errorf("HealthSuspended mismatch")
	}

	// OperationPhase constants
	if canonical.OperationIdle != shim.OperationIdle {
		t.Errorf("OperationIdle mismatch")
	}
	if canonical.OperationSyncing != shim.OperationSyncing {
		t.Errorf("OperationSyncing mismatch")
	}
	if canonical.OperationWaiting != shim.OperationWaiting {
		t.Errorf("OperationWaiting mismatch")
	}

	// ActionType constants
	if canonical.ActionCreate != shim.ActionCreate {
		t.Errorf("ActionCreate mismatch")
	}
	if canonical.ActionUpdate != shim.ActionUpdate {
		t.Errorf("ActionUpdate mismatch")
	}
	if canonical.ActionDelete != shim.ActionDelete {
		t.Errorf("ActionDelete mismatch")
	}
	if canonical.ActionSkip != shim.ActionSkip {
		t.Errorf("ActionSkip mismatch")
	}

	// ResourceMode constants
	if canonical.ModeManaged != shim.ModeManaged {
		t.Errorf("ModeManaged mismatch")
	}
	if canonical.ModeUnmanaged != shim.ModeUnmanaged {
		t.Errorf("ModeUnmanaged mismatch")
	}
	if canonical.ModeData != shim.ModeData {
		t.Errorf("ModeData mismatch")
	}

	// ApplyStatus constants
	if canonical.ApplySucceeded != shim.ApplySucceeded {
		t.Errorf("ApplySucceeded mismatch")
	}
	if canonical.ApplyFailed != shim.ApplyFailed {
		t.Errorf("ApplyFailed mismatch")
	}
	if canonical.ApplySkipped != shim.ApplySkipped {
		t.Errorf("ApplySkipped mismatch")
	}

	// Event constants
	if canonical.EventPlanStart != shim.EventPlanStart {
		t.Errorf("EventPlanStart mismatch")
	}
	if canonical.EventPlanComplete != shim.EventPlanComplete {
		t.Errorf("EventPlanComplete mismatch")
	}
	if canonical.EventApplyStart != shim.EventApplyStart {
		t.Errorf("EventApplyStart mismatch")
	}
	if canonical.EventApplyAction != shim.EventApplyAction {
		t.Errorf("EventApplyAction mismatch")
	}
	if canonical.EventApplyComplete != shim.EventApplyComplete {
		t.Errorf("EventApplyComplete mismatch")
	}
	if canonical.EventDrift != shim.EventDrift {
		t.Errorf("EventDrift mismatch")
	}
	if canonical.EventError != shim.EventError {
		t.Errorf("EventError mismatch")
	}

	// Acknowledgment / Review constants
	if canonical.AckReadDistinct != shim.AckReadDistinct {
		t.Errorf("AckReadDistinct mismatch")
	}
	if canonical.AckAmendInstead != shim.AckAmendInstead {
		t.Errorf("AckAmendInstead mismatch")
	}
	if canonical.ReviewActionAuthored != shim.ReviewActionAuthored {
		t.Errorf("ReviewActionAuthored mismatch")
	}
	if canonical.ReviewActionAmended != shim.ReviewActionAmended {
		t.Errorf("ReviewActionAmended mismatch")
	}
	if canonical.ReviewActionAbandoned != shim.ReviewActionAbandoned {
		t.Errorf("ReviewActionAbandoned mismatch")
	}
}

// TestSummaryHelpers verifies that re-exported method-bearing types work
// correctly. Since Summary is an alias, its methods are available unchanged.
func TestSummaryHelpers(t *testing.T) {
	s := shim.Summary{Creates: 2, Updates: 1, Deletes: 0, Skipped: 3}
	if got := s.Total(); got != 6 {
		t.Errorf("Total() = %d, want 6", got)
	}
	if !s.HasChanges() {
		t.Errorf("HasChanges() = false, want true")
	}

	empty := shim.Summary{}
	if empty.HasChanges() {
		t.Errorf("empty Summary HasChanges() = true, want false")
	}
}

// TestNewResourceStatus verifies the re-exported constructor sets defaults.
func TestNewResourceStatus(t *testing.T) {
	rs := shim.NewResourceStatus(shim.SyncStatusSynced, shim.HealthHealthy)
	if rs.Sync != shim.SyncStatusSynced {
		t.Errorf("Sync = %v, want Synced", rs.Sync)
	}
	if rs.Health != shim.HealthHealthy {
		t.Errorf("Health = %v, want Healthy", rs.Health)
	}
	if rs.Operation != shim.OperationIdle {
		t.Errorf("Operation = %v, want Idle", rs.Operation)
	}
}

// TestGenerateLineage verifies that re-exported GenerateLineage produces
// non-empty, non-identical successive values.
func TestGenerateLineage(t *testing.T) {
	a := shim.GenerateLineage()
	b := shim.GenerateLineage()
	if a == "" {
		t.Errorf("GenerateLineage() returned empty string")
	}
	if a == b {
		t.Errorf("GenerateLineage() returned identical consecutive values: %q", a)
	}
}

// TestDefaultCogdocReviewClass verifies the re-exported constructor has sane
// defaults.
func TestDefaultCogdocReviewClass(t *testing.T) {
	cls := shim.DefaultCogdocReviewClass()
	if !cls.Enabled {
		t.Errorf("DefaultCogdocReviewClass().Enabled = false, want true")
	}
	if cls.SimilarityThreshold != 0.70 {
		t.Errorf("SimilarityThreshold = %v, want 0.70", cls.SimilarityThreshold)
	}
	if cls.TopN != 5 {
		t.Errorf("TopN = %v, want 5", cls.TopN)
	}
}

// TestResourceIndex verifies re-exported helper builds index correctly.
func TestResourceIndex(t *testing.T) {
	state := &shim.State{
		Resources: []shim.Resource{
			{Address: "discord://server1", ExternalID: "ext1"},
			{Address: "discord://server2", ExternalID: "ext2"},
		},
	}
	idx := shim.ResourceIndex(state)
	if len(idx) != 2 {
		t.Errorf("ResourceIndex len = %d, want 2", len(idx))
	}
	if _, ok := idx["discord://server1"]; !ok {
		t.Errorf("missing address 'discord://server1'")
	}

	byExt := shim.ResourceByExternalID(state)
	if len(byExt) != 2 {
		t.Errorf("ResourceByExternalID len = %d, want 2", len(byExt))
	}
	if _, ok := byExt["ext1"]; !ok {
		t.Errorf("missing external_id 'ext1'")
	}
}
