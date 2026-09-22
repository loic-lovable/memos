package shrimp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHumanAttributesRemainDisposableAndUsePinnedAliases(t *testing.T) {
	disabled, err := experimentalHumanProfile(false)
	require.NoError(t, err)
	require.Nil(t, disabled)
	profile, err := experimentalHumanProfile(true)
	if !experimentalHumanAttributesAvailable {
		require.Error(t, err)
		require.Nil(t, profile)
		return
	}
	require.NoError(t, err)
	for _, zone := range []string{"Europe/Stockholm", "US/Eastern", "Factory", "UTC"} {
		values, err := profile.DecodeValues([]byte(`{"timezone":"` + zone + `"}`))
		require.NoError(t, err)
		require.Equal(t, zone, *values.Timezone)
	}
	_, err = profile.DecodeValues([]byte(`{"timezone":"Etc/Unknown"}`))
	require.Error(t, err)
}
