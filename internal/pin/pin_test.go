package pin

import (
	"strings"
	"testing"
)

func TestCaretRangeAcceptsPatchRejectsMajorAndLatest(t *testing.T) {
	sel, err := New("^3.1.0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Allows("3.1.1") {
		t.Fatal("^3.1.0 must accept 3.1.1")
	}
	if !sel.Allows("3.9.0") {
		t.Fatal("^3.1.0 must accept 3.9.0")
	}
	if sel.Allows("4.0.0") {
		t.Fatal("^3.1.0 must reject 4.0.0")
	}
	if sel.Allows("latest") {
		t.Fatal("^3.1.0 must reject latest")
	}
	if sel.Allows("3") {
		t.Fatal("^3.1.0 must reject floating tag 3")
	}
	if !sel.Allows("3.1") {
		t.Fatal("^3.1.0 must accept two-part tag 3.1 as 3.1.0")
	}
	if sel.Allows("beta") {
		t.Fatal("^3.1.0 must reject beta")
	}
	if sel.Allows("rc") {
		t.Fatal("^3.1.0 must reject rc")
	}
}

func TestCaretNewest(t *testing.T) {
	sel, err := New("^3.1.0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, changed := sel.Choose("3.1.0", []string{"latest", "4.0.0", "3.1.1", "3.1.0", "3.2.0", "beta"})
	if !changed || got != "3.2.0" {
		t.Fatalf("got %q changed=%v, want 3.2.0 true", got, changed)
	}
	got, changed = sel.Choose("3.2.0", []string{"3.1.1", "3.2.0", "4.0.0"})
	if changed {
		t.Fatalf("expected noop, got %q", got)
	}
}

func TestTildeRangePatchesOnly(t *testing.T) {
	sel, err := New("~3.1.0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Allows("3.1.9") {
		t.Fatal("~3.1.0 must accept 3.1.9")
	}
	if sel.Allows("3.2.0") {
		t.Fatal("~3.1.0 must reject 3.2.0")
	}
}

func TestExactRangeNeverBumps(t *testing.T) {
	sel, err := New("3.1.0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Allows("3.1.0") {
		t.Fatal("exact 3.1.0 must allow itself")
	}
	if sel.Allows("3.1.1") {
		t.Fatal("exact 3.1.0 must reject 3.1.1")
	}
	_, changed := sel.Choose("3.1.0", []string{"3.1.0", "3.1.1", "3.2.0"})
	if changed {
		t.Fatal("exact pin must never bump")
	}
}

func TestRegexOnlyInclude(t *testing.T) {
	sel, err := New("", `^v2026\.\d+$`, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Allows("v2026.8") {
		t.Fatal("regex must accept v2026.8")
	}
	if sel.Allows("v2025.1") {
		t.Fatal("regex must reject v2025.1")
	}
	if sel.Allows("v2026.8.1") {
		t.Fatal("regex must reject extra components")
	}
	got, changed := sel.Choose("v2026.1", []string{"v2025.1", "v2026.8", "v2026.10", "latest"})
	if !changed || got != "v2026.10" {
		t.Fatalf("got %q changed=%v, want v2026.10 (version-sort, 10 > 8)", got, changed)
	}
}

func TestRangeAndIncludeBothRequired(t *testing.T) {
	sel, err := New("^3.1.0", `^3\.1\.\d+$`, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Allows("3.1.9") {
		t.Fatal("must accept tag that matches range AND include")
	}
	if sel.Allows("3.2.0") {
		t.Fatal("3.2.0 matches ^3.1.0 but not include ^3\\.1\\.\\d+$")
	}
	if sel.Allows("latest") {
		t.Fatal("latest must fail both filters")
	}
}

func TestExclude(t *testing.T) {
	sel, err := New("^2.0.0", "", `.*-rc.*`)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Allows("2.1.0-rc.1") {
		t.Fatal("exclude should drop -rc tags")
	}
	if !sel.Allows("2.1.0") {
		t.Fatal("2.1.0 should remain allowed")
	}
}

func TestCompareVersionish(t *testing.T) {
	if CompareVersionish("v2026.8", "v2026.10") >= 0 {
		t.Fatal("v2026.10 should be newer than v2026.8")
	}
	if CompareVersionish("1.0.0", "1.0") <= 0 {
		t.Fatal("1.0.0 should be greater than 1.0")
	}
}

func TestInvalidSpecs(t *testing.T) {
	if _, err := New("", "", ""); err == nil {
		t.Fatal("empty selector should error")
	}
	if _, err := New("^not a range", "", ""); err == nil {
		t.Fatal("bad range should error")
	}
	if _, err := New("", "(", ""); err == nil {
		t.Fatal("bad include should error")
	}
}

func TestTwoPartTagsMatchRangeAsPatchZero(t *testing.T) {
	pg, err := New("^15.0.0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !pg.Allows("15.19") {
		t.Fatal("^15.0.0 must accept postgres-style 15.19")
	}
	if !pg.Allows("15.20") {
		t.Fatal("^15.0.0 must accept 15.20")
	}
	if pg.Allows("16.0") {
		t.Fatal("^15.0.0 must reject 16.0")
	}
	if pg.Allows("15") {
		t.Fatal("^15.0.0 must reject floating major 15")
	}
	got, changed := pg.Choose("15.19", []string{"15", "16.0", "15.19", "15.20", "latest"})
	if !changed || got != "15.20" {
		t.Fatalf("got %q changed=%v, want 15.20 true", got, changed)
	}

	rd, err := New("^7.0.0", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Allows("7.4.11") {
		t.Fatal("^7.0.0 must still accept 7.4.11")
	}
	if rd.Allows("8.0.0") {
		t.Fatal("^7.0.0 must reject 8.0.0")
	}
	got, changed = rd.Choose("7.4.10", []string{"7.4.11", "8.0.0", "latest"})
	if !changed || got != "7.4.11" {
		t.Fatalf("got %q changed=%v, want 7.4.11 true", got, changed)
	}
}

func TestFollowOnlyValidAndFollowMode(t *testing.T) {
	sel, err := FromLabels(Labels{Follow: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if !sel.FollowMode() || sel.Follow != "latest" {
		t.Fatalf("follow-only must be FollowMode, got %+v", sel)
	}
	if !sel.Active() {
		t.Fatal("follow-only must be active")
	}
}

func TestRangeWinsFollowIgnored(t *testing.T) {
	sel, err := FromLabels(Labels{Range: "^3.1.0", Follow: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.FollowMode() {
		t.Fatal("range + follow must not be FollowMode (range wins)")
	}
	if sel.Follow != "latest" {
		t.Fatalf("follow should still be stored, got %q", sel.Follow)
	}
	if sel.Allows("latest") {
		t.Fatal("range must still reject latest; follow must not semver-sort it")
	}
	got, changed := sel.Choose("3.1.0", []string{"latest", "3.1.1", "main"})
	if !changed || got != "3.1.1" {
		t.Fatalf("range must pick 3.1.1, got %q changed=%v", got, changed)
	}
}

func TestIncludeWinsFollowIgnored(t *testing.T) {
	sel, err := FromLabels(Labels{Include: `^v2026\.\d+$`, Follow: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.FollowMode() {
		t.Fatal("include + follow must not be FollowMode")
	}
}

func TestExcludeOnlyRejected(t *testing.T) {
	_, err := New("", "", ".*-rc.*")
	if err == nil || !strings.Contains(err.Error(), "exclude requires") {
		t.Fatalf("exclude-only must be invalid config, got %v", err)
	}
	if _, err := New("^1.0.0", "", ".*-rc.*"); err != nil {
		t.Fatalf("exclude with range must be allowed: %v", err)
	}
	if _, err := New("", `^v`, ".*-rc.*"); err != nil {
		t.Fatalf("exclude with include must be allowed: %v", err)
	}
}
