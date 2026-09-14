package db_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/db"
)

func TestParseUUID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		ok      bool
		version int
	}{
		{"a UUIDv7, which is what clients generate", "0190b3a1-4d2c-7f1e-8abc-0123456789ab", true, 7},
		{"a UUIDv4 is carried too", "6ba7b810-9dad-41d1-80b4-00c04fd430c8", true, 4},
		{"upper case is the same identifier", "0190B3A1-4D2C-7F1E-8ABC-0123456789AB", true, 7},
		{"the nil UUID", "00000000-0000-0000-0000-000000000000", true, 0},
		{"the unhyphenated form is refused", "0190b3a14d2c7f1e8abc0123456789ab", false, 0},
		{"braces are refused", "{0190b3a1-4d2c-7f1e-8abc-0123456789ab}", false, 0},
		{"a urn prefix is refused", "urn:uuid:0190b3a1-4d2c-7f1e-8abc-0123456789ab", false, 0},
		{"hyphens in the wrong places", "0190b3a1-4d2c7f1e-8abc-0123-456789ab", false, 0},
		{"a digit that is not hex", "0190b3a1-4d2c-7f1e-8abc-0123456789zz", false, 0},
		{"too short", "0190b3a1-4d2c-7f1e-8abc", false, 0},
		{"empty", "", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			got, err := db.ParseUUID(tc.in)
			if !tc.ok {
				r.ErrorIs(err, db.ErrBadUUID)
				return
			}
			r.NoError(err)
			r.Equal(tc.version, got.Version())
			r.Equal(strings.ToLower(tc.in), got.String(), "the canonical spelling is lower case")

			again, err := db.ParseUUID(got.String())
			r.NoError(err)
			r.Equal(got, again, "rendering and parsing are inverses")
		})
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an ordinary name", "Ana Ruiz", "ana-ruiz"},
		{"an accent keeps its base letter", "Escudería Norte", "escuderia-norte"},
		{"so the accented and unaccented spellings are one person", "Escuderia Norte", "escuderia-norte"},
		{"punctuation becomes a separator", "O'Brien-Smith", "o-brien-smith"},
		{"runs of separators collapse", "Ana   Ruiz", "ana-ruiz"},
		{"leading and trailing separators go", "  Ana Ruiz  ", "ana-ruiz"},
		{"digits are kept", "Car 17 Driver", "car-17-driver"},
		{"a name with nothing a web address can carry", "!!!", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := require.New(t)
			got := db.Slug(tc.in)
			r.Equal(tc.want, got)
			if got != "" {
				r.Equal(got, db.Slug(got), "a slug of a slug is the same slug")
			}
		})
	}
}
