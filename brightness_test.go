package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func TestNewBrightness(t *testing.T) {
	b := deskconn.NewBrightness()
	require.NotNil(t, b)
}

func TestGetBrightness(t *testing.T) {
	b := deskconn.NewBrightness()

	value, err := b.GetBrightness()

	if err != nil {
		require.EqualError(t, err, "brightness not available")
		return
	}

	require.GreaterOrEqual(t, value, 0)
	require.LessOrEqual(t, value, 100)
}

func TestSetBrightness(t *testing.T) {
	b := deskconn.NewBrightness()

	tests := []struct {
		name  string
		input int
	}{
		{"normal", 50},
		{"zero", 0},
		{"over", 150},
		{"negative", -10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := b.SetBrightness(tt.input)

			if err != nil {
				require.EqualError(t, err, "failed to set brightness")
				return
			}

			v, err := b.GetBrightness()
			require.NoError(t, err)

			require.GreaterOrEqual(t, v, 1)
			require.LessOrEqual(t, v, 100)
		})
	}
}

func TestBrightnessUnavailable(t *testing.T) {
	b := deskconn.NewBrightness()

	_, errGet := b.GetBrightness()
	errSet := b.SetBrightness(50)

	if errGet != nil {
		require.EqualError(t, errGet, "brightness not available")
		require.EqualError(t, errSet, "failed to set brightness")
	}
}
