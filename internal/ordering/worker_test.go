package ordering

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackoffGrowsWithJitterUpToItsCeiling(t *testing.T) {
	next := backoff(time.Second, 10*time.Second)
	for attempts, ceiling := range map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 3: 8 * time.Second, 4: 10 * time.Second, 50: 10 * time.Second} {
		for range 20 {
			wait := next(attempts)
			require.GreaterOrEqual(t, wait, ceiling/2, attempts)
			require.LessOrEqual(t, wait, ceiling, attempts)
		}
	}
}
