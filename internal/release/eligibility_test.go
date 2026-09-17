package release

import (
	"strings"
	"testing"
	"time"
)

func baseTime() time.Time { return time.Unix(1700000000, 0) }

func okRun(name string, suite int64, started time.Time, id int64) CheckRun {
	return CheckRun{ID: id, Name: name, Status: "completed", Conclusion: "success", AppID: 1, SuiteID: suite, StartedAt: started}
}

func TestCheckEligibilityFailedRerunIsEligible(t *testing.T) {
	t0 := baseTime()
	runs := []CheckRun{
		{ID: 1, Name: "Tests", Status: "completed", Conclusion: "failure", AppID: 1, SuiteID: 10, StartedAt: t0},
		{ID: 2, Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10, StartedAt: t0.Add(time.Minute)},
		okRun("CVE scan", 11, t0, 3),
	}
	res, err := checkEligibility([]string{"Tests", "CVE scan"}, runs)
	if err != nil {
		t.Fatalf("successful rerun after failure must be eligible: %v", err)
	}
	if len(res) != 2 {
		t.Errorf("results = %+v", res)
	}
}

func TestCheckEligibilityCurrentFailureNotMaskedByOlderSuccess(t *testing.T) {
	t0 := baseTime()
	runs := []CheckRun{
		{ID: 1, Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10, StartedAt: t0},
		{ID: 2, Name: "Tests", Status: "completed", Conclusion: "failure", AppID: 1, SuiteID: 10, StartedAt: t0.Add(time.Minute)},
		okRun("CVE scan", 11, t0, 3),
	}
	if _, err := checkEligibility([]string{"Tests", "CVE scan"}, runs); err == nil {
		t.Error("current failure masked by older success must stay ineligible")
	}
}

func TestCheckEligibilityDistinctCurrentProducersAmbiguous(t *testing.T) {
	t0 := baseTime()
	runs := []CheckRun{
		okRun("Tests", 10, t0, 1),
		okRun("Tests", 99, t0.Add(time.Minute), 2),
		okRun("CVE scan", 11, t0, 3),
	}
	if _, err := checkEligibility([]string{"Tests", "CVE scan"}, runs); err == nil {
		t.Fatal("distinct current producers must stay ambiguous")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestCheckEligibilityMissingCheckFailsClosed(t *testing.T) {
	t0 := baseTime()
	if _, err := checkEligibility([]string{"Tests"}, []CheckRun{okRun("Other", 10, t0, 1)}); err == nil {
		t.Error("missing check must fail closed")
	}
}

func TestCheckEligibilityUnconcludedFailsClosed(t *testing.T) {
	t0 := baseTime()
	runs := []CheckRun{{ID: 1, Name: "Tests", Status: "in_progress", Conclusion: "", AppID: 1, SuiteID: 10, StartedAt: t0}}
	if _, err := checkEligibility([]string{"Tests"}, runs); err == nil {
		t.Error("unconcluded check must fail closed")
	}
}

// TestCheckEligibilitySkippedFailsClosed pins the promotion-aware CI
// asymmetry (docs/promotion-ci-plan.md): GitHub branch protection accepts
// conclusion "skipped" for required checks, but eligibility accepts only
// "success" — so a promotion merge commit whose qualification jobs were
// skipped can never masquerade as a qualified source revision.
func TestCheckEligibilitySkippedFailsClosed(t *testing.T) {
	t0 := baseTime()
	runs := []CheckRun{{ID: 1, Name: "Tests", Status: "completed", Conclusion: "skipped", AppID: 1, SuiteID: 10, StartedAt: t0}}
	if _, err := checkEligibility([]string{"Tests"}, runs); err == nil {
		t.Fatal("skipped check must fail closed: skipped satisfies branch protection but is not qualification")
	} else if !strings.Contains(err.Error(), "only success is eligible") {
		t.Errorf("wrong error: %v", err)
	}
	neutral := []CheckRun{{ID: 1, Name: "Tests", Status: "completed", Conclusion: "neutral", AppID: 1, SuiteID: 10, StartedAt: t0}}
	if _, err := checkEligibility([]string{"Tests"}, neutral); err == nil {
		t.Error("neutral check must fail closed")
	}
}
