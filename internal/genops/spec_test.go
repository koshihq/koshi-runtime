package genops

import (
	"fmt"
	"sort"
	"testing"
)

// TestGenOpsSpec_Version guards against accidental spec version changes.
func TestGenOpsSpec_Version(t *testing.T) {
	if SpecVersion != "0.1.0" {
		t.Fatalf("SpecVersion = %q, want %q", SpecVersion, "0.1.0")
	}
}

// TestGenOpsSpec_RequiredAttributes guards the declared v0.1.0 required
// attribute list (Section 7.1) against accidental drift.
func TestGenOpsSpec_RequiredAttributes(t *testing.T) {
	want := []string{
		"genops.accounting.actual",
		"genops.accounting.reserved",
		"genops.accounting.unit",
		"genops.environment",
		"genops.operation.name",
		"genops.operation.type",
		"genops.policy.reason_code",
		"genops.policy.result",
		"genops.project",
		"genops.spec.version",
		"genops.team",
	}

	got := RequiredAttributes[:]
	if len(got) != len(want) {
		t.Fatalf("RequiredAttributes length = %d, want %d\ngot:  %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RequiredAttributes[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Verify sorted (stable diffs).
	if !sort.StringsAreSorted(got) {
		t.Errorf("RequiredAttributes is not sorted: %v", got)
	}

	// Verify no duplicates.
	seen := make(map[string]bool, len(got))
	for _, a := range got {
		if seen[a] {
			t.Errorf("duplicate RequiredAttribute: %q", a)
		}
		seen[a] = true
	}
}

// TestGenOpsSpec_RequiredEvents guards the declared v0.1.0 required
// event list (Section 7.2) against accidental drift.
func TestGenOpsSpec_RequiredEvents(t *testing.T) {
	want := []string{
		"genops.budget.reconciliation",
		"genops.budget.reservation",
		"genops.policy.evaluated",
	}

	got := RequiredEvents[:]
	if len(got) != len(want) {
		t.Fatalf("RequiredEvents length = %d, want %d\ngot:  %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RequiredEvents[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if !sort.StringsAreSorted(got) {
		t.Errorf("RequiredEvents is not sorted: %v", got)
	}

	seen := make(map[string]bool, len(got))
	for _, e := range got {
		if seen[e] {
			t.Errorf("duplicate RequiredEvent: %q", e)
		}
		seen[e] = true
	}
}

// TestGenOpsSpec_AttributeCount ensures the count matches the spec.
func TestGenOpsSpec_AttributeCount(t *testing.T) {
	const wantCount = 11 // GenOps v0.1.0 Section 7.1
	if got := len(RequiredAttributes); got != wantCount {
		t.Errorf("RequiredAttributes count = %d, want %d (GenOps v0.1.0 Section 7.1)",
			got, wantCount)
	}
}

// TestGenOpsSpec_EventCount ensures the count matches the spec.
func TestGenOpsSpec_EventCount(t *testing.T) {
	const wantCount = 3 // GenOps v0.1.0 Section 7.2
	if got := len(RequiredEvents); got != wantCount {
		t.Errorf("RequiredEvents count = %d, want %d (GenOps v0.1.0 Section 7.2)",
			got, wantCount)
	}
}

// TestGenOpsSpec_VersionFormat verifies SemVer format (no "v" prefix).
func TestGenOpsSpec_VersionFormat(t *testing.T) {
	if len(SpecVersion) == 0 {
		t.Fatal("SpecVersion is empty")
	}
	if SpecVersion[0] == 'v' {
		t.Errorf("SpecVersion = %q, must not have 'v' prefix (SemVer format)", SpecVersion)
	}
	// Basic SemVer: at least two dots.
	dots := 0
	for _, c := range SpecVersion {
		if c == '.' {
			dots++
		}
	}
	if dots != 2 {
		t.Errorf("SpecVersion = %q, expected MAJOR.MINOR.PATCH format (2 dots, got %d)", SpecVersion, dots)
	}
}

func TestGenOpsSpec_AllAttributesHavePrefix(t *testing.T) {
	for i, a := range RequiredAttributes {
		if len(a) < 7 || a[:7] != "genops." {
			t.Errorf("RequiredAttributes[%d] = %q, missing genops. prefix", i, a)
		}
	}
}

func TestGenOpsSpec_AllEventsHavePrefix(t *testing.T) {
	for i, e := range RequiredEvents {
		if len(e) < 7 || e[:7] != "genops." {
			t.Errorf("RequiredEvents[%d] = %q, missing genops. prefix", i, e)
		}
	}
}

// TestGenOpsSpec_Summary prints a summary for CI output readability.
func TestGenOpsSpec_Summary(t *testing.T) {
	t.Logf("GenOps spec version: %s", SpecVersion)
	t.Logf("Required attributes (%d):", len(RequiredAttributes))
	for _, a := range RequiredAttributes {
		t.Logf("  %s", a)
	}
	t.Logf("Required events (%d):", len(RequiredEvents))
	for _, e := range RequiredEvents {
		t.Logf("  %s", e)
	}
	_ = fmt.Sprintf("suppress unused import")
}
