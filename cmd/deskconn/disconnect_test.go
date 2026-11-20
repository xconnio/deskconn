package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
)

func TestParseDisconnect(t *testing.T) {
	realm, all, err := parseDisconnect(map[string]any{common.DisconnectKwargRealm: "machine-realm"})
	require.NoError(t, err)
	require.Equal(t, "machine-realm", realm)
	require.False(t, all)

	realm, all, err = parseDisconnect(map[string]any{common.DisconnectKwargAll: true})
	require.NoError(t, err)
	require.Empty(t, realm)
	require.True(t, all)

	for name, kwargs := range map[string]map[string]any{
		"both":          {common.DisconnectKwargRealm: "machine-realm", common.DisconnectKwargAll: true},
		"neither":       {},
		"nil":           nil,
		"all false":     {common.DisconnectKwargAll: false},
		"all not bool":  {common.DisconnectKwargAll: "yes"},
		"empty realm":   {common.DisconnectKwargRealm: ""},
		"realm not str": {common.DisconnectKwargRealm: 7},
	} {
		_, _, err := parseDisconnect(kwargs)
		require.Error(t, err, name)
	}
}
