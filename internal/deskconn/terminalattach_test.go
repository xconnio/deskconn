package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/deskconn"
)

func TestDetachKeys(t *testing.T) {
	tests := []struct {
		name   string
		reads  []string
		out    string
		detach bool
	}{
		{"plain keys", []string{"ls\r"}, "ls\r", false},
		{"detach", []string{"ab\x1dd"}, "ab", true},
		{"split across reads", []string{"ab\x1d", "d"}, "ab", true},
		{"twice sends one", []string{"\x1d\x1dd"}, "\x1dd", false},
		{"another key sends both", []string{"\x1dx", "y"}, "\x1dxy", false},
		{"ctrl+c after it", []string{"\x1d\x03"}, "\x1d\x03", false},
		{"kitty keyboard protocol", []string{"\x1b[93;5u\x1b[100u"}, "", true},
		{"modifyOtherKeys", []string{"\x1b[27;5;93~d"}, "", true},
		{"other escape sequences", []string{"\x1b[A\x1b[93;5uq"}, "\x1b[A\x1b[93;5uq", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var keys deskconn.DetachKeys
			var out []byte
			detach := false
			for _, read := range tt.reads {
				sent, d := keys.Filter([]byte(read))
				out = append(out, sent...)
				if d {
					detach = true
					break
				}
			}
			require.Equal(t, tt.out, string(out))
			require.Equal(t, tt.detach, detach)
		})
	}
}
