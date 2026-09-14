package admin_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/admin"
)

// The address an operator may stamp into a client.
//
// It is the client's own rule applied on this page, so that a mistake is caught
// while the operator is looking at the field rather than by a driver whose app
// will not start. Every refusal below is a sentence that says what to do, not
// what went wrong: the operator is typing an address, not debugging a parser.
func TestClientAddress(t *testing.T) {
	t.Parallel()

	t.Run("what is accepted", func(t *testing.T) {
		t.Parallel()
		//nolint:gocritic // mapKey: the surrounding spaces are what this case is for.
		cases := map[string]string{
			"https://pacenote.example.com":    "https://pacenote.example.com",
			" https://pacenote.example.com/ ": "https://pacenote.example.com",
			// A host with no scheme is filled in rather than refused, because
			// typing a host name is what an operator does.
			"pacenote.example.com":      "https://pacenote.example.com",
			"pacenote.example.com:8443": "https://pacenote.example.com:8443",
			// Except on the driver's own machine, where plain HTTP is the
			// working answer and HTTPS is the broken one.
			"localhost:8080": "http://localhost:8080",
			"localhost":      "http://localhost",
			"127.0.0.1:8080": "http://127.0.0.1:8080",
			"[::1]:8080":     "http://[::1]:8080",
		}
		for typed, want := range cases {
			got, err := admin.ClientAddress(typed)
			require.NoError(t, err, "%q was refused", typed)
			require.Equal(t, want, got, "%q", typed)
		}
	})

	cases := []struct {
		name  string
		typed string
		want  string
	}{
		{name: "nothing at all", typed: "   ", want: "needs a server address"},
		{
			name: "not an address", typed: "https://exa mple.com/\x7f",
			want: "not an address this server can read",
		},
		{name: "a protocol a client does not speak", typed: "ftp://pacenote.example.com", want: "ftp is not"},
		{name: "a scheme and no host", typed: "https://", want: "names no host"},
		{
			name: "credentials in the address", typed: "https://ana:secret@pacenote.example.com",
			want: "carries no user name or password",
		},
		{
			name: "a path as well as a host", typed: "https://pacenote.example.com/admin",
			want: "drop everything after the host name",
		},
		{
			name: "a query on the end", typed: "https://pacenote.example.com?league=gt3",
			want: "carries no query or fragment",
		},
		{
			name: "a fragment on the end", typed: "https://pacenote.example.com#drivers",
			want: "carries no query or fragment",
		},
		{
			// The one that would produce a client that refuses to start. It is
			// worth a sentence saying so rather than a generic refusal.
			name:  "plain HTTP to somewhere that is not this machine",
			typed: "http://pacenote.example.com", want: "the client will refuse to start",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)

			got, err := admin.ClientAddress(tc.typed)
			r.Error(err, "%q was accepted", tc.typed)
			r.Empty(got)
			r.Contains(err.Error(), tc.want)
		})
	}
}
