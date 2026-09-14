package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// IdempotencyTTL is how long a stored response is replayed for. Twenty-four
// hours covers a client that was offline for a session and drained its queue
// the next morning, which is the case the table exists for.
const IdempotencyTTL = 24 * time.Hour

// ErrIdempotencyConflict reports an idempotency key presented a second time
// with a different request. The contract calls that a bug in the caller: it is
// never retried and the stored response is not touched.
var ErrIdempotencyConflict = errors.New("db: that idempotency key was used with a different request")

// Response is what a write answered with: the status and the exact bytes. Both
// are stored, so a replay is byte-for-byte the first answer rather than a
// second answer that happens to look similar.
type Response struct {
	Status int
	Body   []byte
}

// Write is one idempotent request, identified by the device that sent it and
// the key it carried, and fingerprinted by Hash.
//
// Hash covers the method, the path and the body. Two requests with the same key
// and the same hash are the same request; the same key with a different hash is
// [ErrIdempotencyConflict].
type Write struct {
	DeviceID int64
	Key      string
	Hash     []byte
	Now      time.Time
}

// Idempotent runs fn exactly once for this device and key and returns what it
// answered, replaying the stored answer on every repeat.
//
// The claim and the work are one transaction, which is what makes a concurrent
// double submit correct rather than merely unlikely: the second request's
// insert blocks on the primary key until the first commits, and then reads the
// committed response and replays it. Neither request ever sees a half-written
// state, and the work happens once.
//
// fn returns a [Response] for anything the client should be told, including a
// refusal — a 409 for a lap that disagrees with one already stored is an answer
// and is itself replayed. It returns an error only for a fault, which rolls the
// whole transaction back, claim included, so the client's retry can succeed.
//
// The second result reports whether the answer was replayed rather than
// produced.
func (s *Store) Idempotent(ctx context.Context, w Write, fn func(context.Context, *Tx) (Response, error)) (Response, bool, error) {
	if w.Now.IsZero() {
		w.Now = time.Now()
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Response{}, false, classify(err, s.pool.Config().ConnConfig)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := s.q.WithTx(tx)
	claimErr := q.ClaimIdempotencyKey(ctx, gen.ClaimIdempotencyKeyParams{
		DeviceID:    w.DeviceID,
		Key:         w.Key,
		RequestHash: w.Hash,
		ExpiresAt:   pgtype.Timestamptz{Time: w.Now.Add(IdempotencyTTL), Valid: true},
	})
	if claimErr != nil {
		if !isUniqueViolation(claimErr) {
			return Response{}, false, fmt.Errorf("db: cannot record the idempotency key: %w", claimErr)
		}
		// The insert blocked on the primary key until the first request
		// committed, so by the time the violation was raised the row is there
		// and readable. Roll this transaction back before reading it: it is
		// aborted, and nothing else in it is wanted.
		_ = tx.Rollback(ctx)
		committed = true
		res, replayErr := s.replay(ctx, w)
		return res, true, replayErr
	}

	res, err := fn(ctx, &Tx{tx: tx, q: q})
	if err != nil {
		return Response{}, false, err
	}
	if err := q.FinishIdempotencyKey(ctx, gen.FinishIdempotencyKeyParams{
		DeviceID:     w.DeviceID,
		Key:          w.Key,
		Status:       int32(res.Status), //nolint:gosec // G115: an HTTP status is three digits.
		ResponseBody: res.Body,
	}); err != nil {
		return Response{}, false, fmt.Errorf("db: cannot store the response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Response{}, false, fmt.Errorf("db: cannot finish the write: %w", err)
	}
	committed = true
	return res, false, nil
}

// replay reads the stored answer for a key that has been seen before, and
// refuses one presented with a different request.
func (s *Store) replay(ctx context.Context, w Write) (Response, error) {
	row, err := s.q.IdempotencyKey(ctx, gen.IdempotencyKeyParams{DeviceID: w.DeviceID, Key: w.Key})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The winner rolled back between the violation and this read, so
			// there is nothing to replay. Saying so lets the caller answer
			// "try again" rather than invent a response.
			return Response{}, ErrNotFound
		}
		return Response{}, fmt.Errorf("db: cannot read the stored response: %w", err)
	}
	if !bytes.Equal(row.RequestHash, w.Hash) {
		return Response{}, ErrIdempotencyConflict
	}
	if row.Status == 0 {
		// A committed claim with no response is a bug in this package rather
		// than in the client, and pretending otherwise would replay an empty
		// body forever.
		return Response{}, errors.New("db: that idempotency key was stored without a response")
	}
	return Response{Status: int(row.Status), Body: row.ResponseBody}, nil
}

// DeleteExpiredIdempotencyKeys removes replay entries that have run out. D-6
// gives them twenty-four hours; this is what enforces it.
func (s *Store) DeleteExpiredIdempotencyKeys(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.q.DeleteExpiredIdempotencyKeys(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("db: cannot clear out the expired idempotency keys: %w", err)
	}
	return n, nil
}
