package metrics

import "testing"

// TestNew_Repeatable is the regression test for the duplicate-registration
// panic: New() must be callable more than once in a process (each builds
// its own registry), which the e2e suite relies on to stand up several
// servers sequentially.
func TestNew_Repeatable(t *testing.T) {
	m1 := New()
	m2 := New() // previously panicked: "duplicate metrics collector registration"
	if m1.Registry == nil || m2.Registry == nil {
		t.Fatal("Registry not set")
	}
	if m1.Registry == m2.Registry {
		t.Fatal("both instances share a registry; they must be independent")
	}
	// The app collectors must be gatherable from the instance registry.
	m1.JobsSubmitted.WithLabelValues("probe").Inc()
	fams, err := m1.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, f := range fams {
		if f.GetName() == "orpheus_jobs_submitted_total" {
			found = true
		}
	}
	if !found {
		t.Error("orpheus_jobs_submitted_total not present in instance registry")
	}
}
