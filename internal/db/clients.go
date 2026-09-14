package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// ClientBuild is one client an operator built: the address that went into the
// binary, the file it produced, and who pressed the button.
type ClientBuild struct {
	ID int64
	At time.Time
	// Actor is the administrator's email address.
	Actor string
	// Address is the server the built client talks to.
	Address string
	// Signing is which of the three routes was taken, as the build page
	// spells it.
	Signing string
	// Reference names the file on the disk, the download address, and the
	// identifier stamped inside the binary.
	Reference string
	// FileName is what a driver downloads, FileBytes its size, and SHA256 the
	// digest an operator tells their drivers to check.
	FileName  string
	FileBytes int64
	SHA256    string
	// ClientVersion is the version of the prebuilt client it was stamped from.
	ClientVersion string
	// SignedBy is the certificate that signed it, empty for a build that was
	// not signed or was made before this server recorded it.
	SignedBy string
}

// Cursor is where a page of the history resumes after this row.
func (b ClientBuild) Cursor() ClientBuildCursor {
	return ClientBuildCursor{At: b.At, ID: b.ID}
}

// ClientBuildCursor is a position in the build history: newest first, so the
// next page is everything older than this row.
type ClientBuildCursor struct {
	At time.Time
	ID int64
}

// ClientBuildQuery asks for one page of the build history.
type ClientBuildQuery struct {
	// After is the cursor, or nil for the first page.
	After *ClientBuildCursor
	// Limit is how many rows to read. Zero is [PageSize].
	Limit int
}

// NewClientBuild is a build to record. It is the page's own vocabulary rather
// than the table's, so that a caller cannot put the digest in the address by
// getting two string arguments the wrong way round.
type NewClientBuild struct {
	Actor         string
	Address       string
	Signing       string
	Reference     string
	FileName      string
	FileBytes     int64
	SHA256        string
	ClientVersion string
	SignedBy      string
}

// RecordClientBuild writes one row of the build history and returns it.
func (s *Store) RecordClientBuild(ctx context.Context, b NewClientBuild) (ClientBuild, error) {
	row, err := s.q.InsertClientBuild(ctx, gen.InsertClientBuildParams{
		Actor:         b.Actor,
		Address:       b.Address,
		Signing:       b.Signing,
		Reference:     b.Reference,
		FileName:      b.FileName,
		FileBytes:     b.FileBytes,
		Sha256:        b.SHA256,
		ClientVersion: b.ClientVersion,
		SignedBy:      b.SignedBy,
	})
	if err != nil {
		return ClientBuild{}, fmt.Errorf("db: cannot record the build: %w", err)
	}
	return clientBuildOf(&row), nil
}

// ClientBuilds reads one page of the build history, newest first.
func (s *Store) ClientBuilds(ctx context.Context, q ClientBuildQuery) ([]ClientBuild, error) {
	after := ClientBuildCursor{}
	if q.After != nil {
		after = *q.After
	}
	rows, err := s.q.ClientBuildsPage(ctx, gen.ClientBuildsPageParams{
		First:   q.After == nil,
		AfterAt: pgtype.Timestamptz{Time: after.At, Valid: true},
		AfterID: after.ID,
		Lim:     int32(pageLimit(q.Limit)), //nolint:gosec // G115: pageLimit bounds the value well inside int32.
	})
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the build history: %w", err)
	}
	out := make([]ClientBuild, 0, len(rows))
	for i := range rows {
		out = append(out, clientBuildOf(&rows[i]))
	}
	return out, nil
}

// ClientBuildByReference reads one build by the reference in its download
// address. A reference nothing was built under is [ErrNotFound].
func (s *Store) ClientBuildByReference(ctx context.Context, reference string) (ClientBuild, error) {
	row, err := s.q.ClientBuildByReference(ctx, reference)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ClientBuild{}, ErrNotFound
		}
		return ClientBuild{}, fmt.Errorf("db: cannot read that build: %w", err)
	}
	return clientBuildOf(&row), nil
}

// CountClientBuilds is how many clients have ever been built here.
func (s *Store) CountClientBuilds(ctx context.Context) (int64, error) {
	n, err := s.q.CountClientBuilds(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: cannot count the builds: %w", err)
	}
	return n, nil
}

func clientBuildOf(row *gen.ClientBuild) ClientBuild {
	return ClientBuild{
		ID:            row.ID,
		At:            row.At.Time,
		Actor:         row.Actor,
		Address:       row.Address,
		Signing:       row.Signing,
		Reference:     row.Reference,
		FileName:      row.FileName,
		FileBytes:     row.FileBytes,
		SHA256:        row.Sha256,
		ClientVersion: row.ClientVersion,
		SignedBy:      row.SignedBy,
	}
}
