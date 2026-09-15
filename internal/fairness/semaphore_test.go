package fairness_test

import (
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/fairness"
	"github.com/stretchr/testify/require"
)

func TestSemaphoreRejectsInvalidAndUsesLeaseShape(t *testing.T) {
	_, err := fairness.NewSemaphore(nil, "")
	require.ErrorIs(t, err, fairness.ErrRedisRequired)
	require.Error(t, fairness.ValidateLimits(-1, 1))
	require.NoError(t, fairness.ValidateLimits(0, 0))
}
