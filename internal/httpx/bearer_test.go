package httpx_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/httpx"
)

func TestBearerToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER   abc  ", "abc", true},
		{"Bearer", "", false},
		{"Bearer   ", "", false},
		{"Basic abc", "", false},
		{"Bearerabc", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			got, ok := httpx.ParseBearer(tc.header)
			r.Equal(tc.ok, ok)
			r.Equal(tc.want, got)

			req := &http.Request{Header: http.Header{}}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got, ok = httpx.BearerToken(req)
			r.Equal(tc.ok, ok)
			r.Equal(tc.want, got)
		})
	}
}
