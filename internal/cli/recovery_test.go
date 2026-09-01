package cli

import (
	"strconv"
	"testing"

	"github.com/restorelab/restorelab/internal/adhoc"
)

// The cobra flag defaults document the CLI's --help output, but they must
// stay equal to internal/adhoc's: a drill launched from the terminal and one
// launched over HTTP must be the same drill.
func TestAdHocFlagDefaultsMatchTheSharedOnes(t *testing.T) {
	cmd := newRecoveryTestCmd(&app{})
	if got := cmd.Flags().Lookup("check-retries").DefValue; got != strconv.Itoa(adhoc.DefaultCheckRetries) {
		t.Errorf("--check-retries default = %s, want %d: a drill from the terminal and one from the API must be the same drill",
			got, adhoc.DefaultCheckRetries)
	}
}
