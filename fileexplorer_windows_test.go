//go:build windows

package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

// TestFileBrowserBrowseBareDriveLetter guards against a real bug: a bare drive letter like
// "C:" (no trailing separator) is a valid Windows absolute path meaning that drive's root, but
// filepath.IsAbs requires the trailing separator to recognize it as such. Without accounting
// for that, "C:" was treated as relative and joined onto the home directory, producing a
// nonsensical path like "C:\Users\Asim\C:" that then failed to resolve at all.
func TestFileBrowserBrowseBareDriveLetter(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse("C:", "", 0)
	require.NoError(t, err)
	require.True(t, result.IsDir)
	require.Equal(t, `C:\`, result.Path)
}
