// Package buildinfo exposes the immutable release identifier set by the linker.
package buildinfo

import (
	"errors"
	"regexp"
)

// Revision is set at build time with -X. Runtime environment cannot replace it.
var Revision = "development"

var revisionPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

// ValidateExpected refuses to run a mismatched image when the controller binds
// execution to a release. Empty expected permits ordinary local development.
func ValidateExpected(expected string) error {
	if expected == "" {
		return nil
	}
	if !revisionPattern.MatchString(expected) || !revisionPattern.MatchString(Revision) || expected != Revision {
		return errors.New("container build revision does not match the expected release")
	}
	return nil
}
