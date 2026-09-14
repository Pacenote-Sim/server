package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// TokenUse is what the language-model features spent over one window.
type TokenUse struct {
	// Input and Output are tokens, counted the way the model counts them.
	Input, Output int64
	// Calls is how many requests those tokens were spread over.
	Calls int64
}

// Total is what the daily cap is measured against: every token the operator
// paid for, in or out.
func (u TokenUse) Total() int64 { return u.Input + u.Output }

// RecordTokenUsage stores what one language-model call cost. It is what makes
// the operator's daily cap enforceable and the figures on the settings page
// true; a call that is not recorded is a call the operator cannot see.
//
// driverID is nil for work that was not on any one driver's behalf.
func (s *Store) RecordTokenUsage(ctx context.Context, job, model string, driverID *int64, input, output int64) error {
	err := s.q.RecordTokenUsage(ctx, gen.RecordTokenUsageParams{
		Job:          job,
		Model:        model,
		DriverID:     driverID,
		InputTokens:  input,
		OutputTokens: output,
	})
	if err != nil {
		return fmt.Errorf("db: cannot record what that call cost: %w", err)
	}
	return nil
}

// TokensSince sums the usage from an instant until now.
func (s *Store) TokensSince(ctx context.Context, since time.Time) (TokenUse, error) {
	row, err := s.q.TokensSince(ctx, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return TokenUse{}, fmt.Errorf("db: cannot read what has been spent: %w", err)
	}
	return TokenUse{Input: row.InputTokens, Output: row.OutputTokens, Calls: row.Calls}, nil
}
