package quota_test

import (
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/quota"
	"github.com/stretchr/testify/require"
)

func TestCounterRequiresRedisAndValidIdentity(t *testing.T) {
	_, err := quota.NewCounter(nil, "")
	require.ErrorIs(t, err, quota.ErrRedisRequired)
}
