package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// Driver is one person who drives. In the community edition there is one
// organisation and no entitlement model, so a driver is an identity and a
// display name and nothing else.
type Driver struct {
	ID         int64
	ExternalID string
	Name       string
	Slug       string
	Class      string
	AvatarURL  string
	CreatedAt  time.Time
}

// DriverByID reads one driver.
func (s *Store) DriverByID(ctx context.Context, id int64) (Driver, error) {
	row, err := s.q.DriverByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Driver{}, ErrNotFound
		}
		return Driver{}, fmt.Errorf("db: cannot read the driver: %w", err)
	}
	return driverOf(row), nil
}

// ListDrivers lists every driver, by name. The admin panel offers them when an
// operator approves a pairing.
func (s *Store) ListDrivers(ctx context.Context) ([]Driver, error) {
	rows, err := s.q.ListDrivers(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: cannot list the drivers: %w", err)
	}
	out := make([]Driver, 0, len(rows))
	for i := range rows {
		out = append(out, driverOf(rows[i]))
	}
	return out, nil
}

// EnsureDriver finds the driver with this name's slug, or creates one. It is
// what approving a pairing calls: an operator types a name, and typing the same
// name twice attaches the second machine to the same person rather than making
// a second person with the same name.
func (s *Store) EnsureDriver(ctx context.Context, name, class string) (Driver, error) {
	slug := Slug(name)
	if slug == "" {
		return Driver{}, errors.New("db: that name has nothing in it a web address could carry")
	}
	row, err := s.q.DriverBySlug(ctx, slug)
	switch {
	case err == nil:
		return driverOf(row), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Driver{}, fmt.Errorf("db: cannot read the driver: %w", err)
	}
	id, err := s.CreateDriver(ctx, "", strings.TrimSpace(name), slug, class, "")
	if err != nil {
		return Driver{}, err
	}
	return s.DriverByID(ctx, id)
}

// Slug is the stable, URL-safe spelling of a driver's name: lower case, letters
// and digits, single hyphens between words. Accented letters keep their base
// letter, so "Escudería" and "Escuderia" do not become two different people.
func Slug(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if base := fold(r); base != 0 {
				if dash && b.Len() > 0 {
					b.WriteByte('-')
				}
				dash = false
				b.WriteRune(base)
			}
		default:
			dash = b.Len() > 0
		}
	}
	return b.String()
}

// fold maps the Latin-1 accented letters onto their base letter and leaves
// everything else alone. It is deliberately small: a full Unicode fold would
// need a table this server has no other use for, and a name it cannot fold
// still produces a usable slug from the letters it can.
func fold(r rune) rune {
	const (
		from = "àáâãäåçèéêëìíîïñòóôõöùúûüýÿ"
		to   = "aaaaaaceeeeiiiinooooouuuuyy"
	)
	if r < unicode.MaxASCII {
		return r
	}
	fs, ts := []rune(from), []rune(to)
	for i, f := range fs {
		if f == r {
			return ts[i]
		}
	}
	return r
}

func driverOf(row gen.Driver) Driver {
	return Driver{
		ID:         row.ID,
		ExternalID: row.ExternalID,
		Name:       row.Name,
		Slug:       row.Slug,
		Class:      row.Class,
		AvatarURL:  row.AvatarUrl,
		CreatedAt:  row.CreatedAt.Time,
	}
}
