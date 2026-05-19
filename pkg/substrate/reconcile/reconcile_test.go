package reconcile_test

import (
	"reflect"
	"testing"

	origin "github.com/myrgic/cogos/pkg/reconcile"
	substrate "github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// TestTypeIdentity verifies that type aliases in the substrate re-export layer
// are identical to their origin types via reflect.TypeOf. Because Go type
// aliases share the same runtime type, these must always be equal.
func TestTypeIdentity(t *testing.T) {
	cases := []struct {
		name    string
		origin  reflect.Type
		reexport reflect.Type
	}{
		{"SyncStatus", reflect.TypeOf(origin.SyncStatus("")), reflect.TypeOf(substrate.SyncStatus(""))},
		{"HealthStatus", reflect.TypeOf(origin.HealthStatus("")), reflect.TypeOf(substrate.HealthStatus(""))},
		{"OperationPhase", reflect.TypeOf(origin.OperationPhase("")), reflect.TypeOf(substrate.OperationPhase(""))},
		{"ActionType", reflect.TypeOf(origin.ActionType("")), reflect.TypeOf(substrate.ActionType(""))},
		{"ResourceMode", reflect.TypeOf(origin.ResourceMode("")), reflect.TypeOf(substrate.ResourceMode(""))},
		{"ApplyStatus", reflect.TypeOf(origin.ApplyStatus("")), reflect.TypeOf(substrate.ApplyStatus(""))},
		{"AcknowledgmentDecision", reflect.TypeOf(origin.AcknowledgmentDecision("")), reflect.TypeOf(substrate.AcknowledgmentDecision(""))},
		{"ReviewActionTaken", reflect.TypeOf(origin.ReviewActionTaken("")), reflect.TypeOf(substrate.ReviewActionTaken(""))},
		{"ResourceStatus", reflect.TypeOf(origin.ResourceStatus{}), reflect.TypeOf(substrate.ResourceStatus{})},
		{"Plan", reflect.TypeOf(origin.Plan{}), reflect.TypeOf(substrate.Plan{})},
		{"Action", reflect.TypeOf(origin.Action{}), reflect.TypeOf(substrate.Action{})},
		{"Summary", reflect.TypeOf(origin.Summary{}), reflect.TypeOf(substrate.Summary{})},
		{"Result", reflect.TypeOf(origin.Result{}), reflect.TypeOf(substrate.Result{})},
		{"State", reflect.TypeOf(origin.State{}), reflect.TypeOf(substrate.State{})},
		{"Resource", reflect.TypeOf(origin.Resource{}), reflect.TypeOf(substrate.Resource{})},
		{"Event", reflect.TypeOf(origin.Event{}), reflect.TypeOf(substrate.Event{})},
		{"MetaResource", reflect.TypeOf(origin.MetaResource{}), reflect.TypeOf(substrate.MetaResource{})},
		{"MetaConfig", reflect.TypeOf(origin.MetaConfig{}), reflect.TypeOf(substrate.MetaConfig{})},
		{"MetaResult", reflect.TypeOf(origin.MetaResult{}), reflect.TypeOf(substrate.MetaResult{})},
		{"MetaOpts", reflect.TypeOf(origin.MetaOpts{}), reflect.TypeOf(substrate.MetaOpts{})},
		{"CogdocReviewClass", reflect.TypeOf(origin.CogdocReviewClass{}), reflect.TypeOf(substrate.CogdocReviewClass{})},
		{"CogdocProposal", reflect.TypeOf(origin.CogdocProposal{}), reflect.TypeOf(substrate.CogdocProposal{})},
		{"SimilarityCandidate", reflect.TypeOf(origin.SimilarityCandidate{}), reflect.TypeOf(substrate.SimilarityCandidate{})},
		{"CandidateAcknowledgment", reflect.TypeOf(origin.CandidateAcknowledgment{}), reflect.TypeOf(substrate.CandidateAcknowledgment{})},
		{"ProvenanceRecord", reflect.TypeOf(origin.ProvenanceRecord{}), reflect.TypeOf(substrate.ProvenanceRecord{})},
		{"ReviewTRMTuple", reflect.TypeOf(origin.ReviewTRMTuple{}), reflect.TypeOf(substrate.ReviewTRMTuple{})},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.origin != tc.reexport {
				t.Errorf("%s: origin type %v != substrate type %v (alias is broken)", tc.name, tc.origin, tc.reexport)
			}
		})
	}
}

// TestConstantIdentity verifies that re-exported constants have the same value
// as their origin counterparts.
func TestConstantIdentity(t *testing.T) {
	// SyncStatus constants
	if origin.SyncStatusSynced != substrate.SyncStatusSynced {
		t.Errorf("SyncStatusSynced mismatch")
	}
	if origin.SyncStatusOutOfSync != substrate.SyncStatusOutOfSync {
		t.Errorf("SyncStatusOutOfSync mismatch")
	}
	if origin.SyncStatusUnknown != substrate.SyncStatusUnknown {
		t.Errorf("SyncStatusUnknown mismatch")
	}

	// HealthStatus constants
	if origin.HealthHealthy != substrate.HealthHealthy {
		t.Errorf("HealthHealthy mismatch")
	}
	if origin.HealthDegraded != substrate.HealthDegraded {
		t.Errorf("HealthDegraded mismatch")
	}
	if origin.HealthProgressing != substrate.HealthProgressing {
		t.Errorf("HealthProgressing mismatch")
	}
	if origin.HealthMissing != substrate.HealthMissing {
		t.Errorf("HealthMissing mismatch")
	}
	if origin.HealthSuspended != substrate.HealthSuspended {
		t.Errorf("HealthSuspended mismatch")
	}

	// OperationPhase constants
	if origin.OperationIdle != substrate.OperationIdle {
		t.Errorf("OperationIdle mismatch")
	}
	if origin.OperationSyncing != substrate.OperationSyncing {
		t.Errorf("OperationSyncing mismatch")
	}
	if origin.OperationWaiting != substrate.OperationWaiting {
		t.Errorf("OperationWaiting mismatch")
	}

	// ActionType constants
	if origin.ActionCreate != substrate.ActionCreate {
		t.Errorf("ActionCreate mismatch")
	}
	if origin.ActionUpdate != substrate.ActionUpdate {
		t.Errorf("ActionUpdate mismatch")
	}
	if origin.ActionDelete != substrate.ActionDelete {
		t.Errorf("ActionDelete mismatch")
	}
	if origin.ActionSkip != substrate.ActionSkip {
		t.Errorf("ActionSkip mismatch")
	}

	// ResourceMode constants
	if origin.ModeManaged != substrate.ModeManaged {
		t.Errorf("ModeManaged mismatch")
	}
	if origin.ModeUnmanaged != substrate.ModeUnmanaged {
		t.Errorf("ModeUnmanaged mismatch")
	}
	if origin.ModeData != substrate.ModeData {
		t.Errorf("ModeData mismatch")
	}

	// ApplyStatus constants
	if origin.ApplySucceeded != substrate.ApplySucceeded {
		t.Errorf("ApplySucceeded mismatch")
	}
	if origin.ApplyFailed != substrate.ApplyFailed {
		t.Errorf("ApplyFailed mismatch")
	}
	if origin.ApplySkipped != substrate.ApplySkipped {
		t.Errorf("ApplySkipped mismatch")
	}

	// Event constants
	if origin.EventPlanStart != substrate.EventPlanStart {
		t.Errorf("EventPlanStart mismatch")
	}
	if origin.EventPlanComplete != substrate.EventPlanComplete {
		t.Errorf("EventPlanComplete mismatch")
	}
	if origin.EventApplyStart != substrate.EventApplyStart {
		t.Errorf("EventApplyStart mismatch")
	}
	if origin.EventApplyAction != substrate.EventApplyAction {
		t.Errorf("EventApplyAction mismatch")
	}
	if origin.EventApplyComplete != substrate.EventApplyComplete {
		t.Errorf("EventApplyComplete mismatch")
	}
	if origin.EventDrift != substrate.EventDrift {
		t.Errorf("EventDrift mismatch")
	}
	if origin.EventError != substrate.EventError {
		t.Errorf("EventError mismatch")
	}

	// Acknowledgment / Review constants
	if origin.AckReadDistinct != substrate.AckReadDistinct {
		t.Errorf("AckReadDistinct mismatch")
	}
	if origin.AckAmendInstead != substrate.AckAmendInstead {
		t.Errorf("AckAmendInstead mismatch")
	}
	if origin.ReviewActionAuthored != substrate.ReviewActionAuthored {
		t.Errorf("ReviewActionAuthored mismatch")
	}
	if origin.ReviewActionAmended != substrate.ReviewActionAmended {
		t.Errorf("ReviewActionAmended mismatch")
	}
	if origin.ReviewActionAbandoned != substrate.ReviewActionAbandoned {
		t.Errorf("ReviewActionAbandoned mismatch")
	}
}

// TestSummaryHelpers verifies that re-exported method-bearing types work correctly.
// Since Summary is an alias, its methods are available unchanged.
func TestSummaryHelpers(t *testing.T) {
	s := substrate.Summary{Creates: 2, Updates: 1, Deletes: 0, Skipped: 3}
	if got := s.Total(); got != 6 {
		t.Errorf("Total() = %d, want 6", got)
	}
	if !s.HasChanges() {
		t.Errorf("HasChanges() = false, want true")
	}

	empty := substrate.Summary{}
	if empty.HasChanges() {
		t.Errorf("empty Summary HasChanges() = true, want false")
	}
}

// TestNewResourceStatus verifies the re-exported constructor sets defaults.
func TestNewResourceStatus(t *testing.T) {
	rs := substrate.NewResourceStatus(substrate.SyncStatusSynced, substrate.HealthHealthy)
	if rs.Sync != substrate.SyncStatusSynced {
		t.Errorf("Sync = %v, want Synced", rs.Sync)
	}
	if rs.Health != substrate.HealthHealthy {
		t.Errorf("Health = %v, want Healthy", rs.Health)
	}
	if rs.Operation != substrate.OperationIdle {
		t.Errorf("Operation = %v, want Idle", rs.Operation)
	}
}

// TestGenerateLineage verifies that re-exported GenerateLineage produces
// non-empty, non-identical successive values.
func TestGenerateLineage(t *testing.T) {
	a := substrate.GenerateLineage()
	b := substrate.GenerateLineage()
	if a == "" {
		t.Errorf("GenerateLineage() returned empty string")
	}
	if a == b {
		t.Errorf("GenerateLineage() returned identical consecutive values: %q", a)
	}
}

// TestDefaultCogdocReviewClass verifies the re-exported constructor has sane defaults.
func TestDefaultCogdocReviewClass(t *testing.T) {
	cls := substrate.DefaultCogdocReviewClass()
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
	state := &substrate.State{
		Resources: []substrate.Resource{
			{Address: "discord://server1", ExternalID: "ext1"},
			{Address: "discord://server2", ExternalID: "ext2"},
		},
	}
	idx := substrate.ResourceIndex(state)
	if len(idx) != 2 {
		t.Errorf("ResourceIndex len = %d, want 2", len(idx))
	}
	if _, ok := idx["discord://server1"]; !ok {
		t.Errorf("missing address 'discord://server1'")
	}

	byExt := substrate.ResourceByExternalID(state)
	if len(byExt) != 2 {
		t.Errorf("ResourceByExternalID len = %d, want 2", len(byExt))
	}
	if _, ok := byExt["ext1"]; !ok {
		t.Errorf("missing external_id 'ext1'")
	}
}
