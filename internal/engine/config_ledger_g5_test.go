// config_ledger_g5_test.go — negative-control test for the IdentityNakedDefault comment.
//
// This test reads config.go as a text file and checks that the comment on
// IdentityNakedDefault reflects the 2026-09-05 census finding (default TRUE,
// naked is the honest default). It FAILS if the old "Default FALSE" wording is
// present and PASSES only after the comment has been corrected.
package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLedgerG5_ConfigIdentityNakedDefaultComment(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	configPath := filepath.Join(filepath.Dir(thisFile), "config.go")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("cannot read config.go: %v", err)
	}
	src := string(data)

	// The new comment must mention the census finding.
	if !strings.Contains(src, "census") {
		t.Error("config.go: IdentityNakedDefault comment does not mention \"census\" — old wording still present or comment was not updated")
	}

	// The old "Default FALSE" wording must be gone.
	if strings.Contains(src, "Default FALSE") {
		t.Error("config.go: old \"Default FALSE\" wording still present in IdentityNakedDefault comment — fix not applied")
	}
}
