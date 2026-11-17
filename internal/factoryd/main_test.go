package factoryd

import (
	"flag"
	"os"
	"testing"
	"time"
)

// The package's four-thousand tests run past the go tool's default
// ten-minute alarm in one binary, as a plain `go test ./...` of the
// repository fires it: hold the alarm at the suite bound the repo's own
// make test passes, never lowering a longer one a caller asked for.
func TestMain(m *testing.M) {
	flag.Parse()
	if d, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String()); err == nil && d < 30*time.Minute {
		flag.Set("test.timeout", "30m0s")
	}
	os.Exit(m.Run())
}
