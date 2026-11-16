package buildinfo

import (
	"strings"
	"testing"
)

func TestExpectedRevisionGuardsExecution(t *testing.T) {
	previous := Revision
	t.Cleanup(func() { Revision = previous })
	Revision = strings.Repeat("a", 40)
	if err := ValidateExpected(Revision); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExpected(""); err != nil {
		t.Fatalf("ordinary local execution rejected: %v", err)
	}
	for _, expected := range []string{strings.Repeat("b", 40), "development", "../image", strings.Repeat("A", 40), "secret-that-must-not-be-reflected"} {
		if err := ValidateExpected(expected); err == nil {
			t.Fatalf("unsafe or mismatched expected revision accepted: %q", expected)
		} else if strings.Contains(err.Error(), "secret-that-must-not-be-reflected") {
			t.Fatal("error reflected a supplied value")
		}
	}
	Revision = "development"
	if err := ValidateExpected(strings.Repeat("a", 40)); err == nil {
		t.Fatal("development binary accepted a production expected revision")
	}
}

func TestFullSHA256RevisionCanBeVerified(t *testing.T) {
	previous := Revision
	t.Cleanup(func() { Revision = previous })
	Revision = strings.Repeat("f", 64)
	if err := ValidateExpected(Revision); err != nil {
		t.Fatal(err)
	}
}
