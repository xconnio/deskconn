package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/deskconn"
)

func TestIsEditableExtension(t *testing.T) {
	require.True(t, deskconn.IsEditableExtension("notes.txt"))
	require.True(t, deskconn.IsEditableExtension("main.go"))
	require.True(t, deskconn.IsEditableExtension("Makefile"))
	require.True(t, deskconn.IsEditableExtension(".gitignore"))

	require.False(t, deskconn.IsEditableExtension("movie.mp4"))
	require.False(t, deskconn.IsEditableExtension("photo.jpg"))
	require.False(t, deskconn.IsEditableExtension("report.pdf"))
	require.False(t, deskconn.IsEditableExtension("resume.docx"))
}
