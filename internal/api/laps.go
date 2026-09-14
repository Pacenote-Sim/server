package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"log/slog"
	"net/http"
	"sync"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/protocol/trace"
	"github.com/pacenote-sim/protocol/wire"
	"github.com/pacenote-sim/server/internal/db"
	"github.com/pacenote-sim/server/internal/httpx"
)

// postLaps appends a batch of laps to a stint.
//
// Append-only and idempotent on (stint, number): a repeated lap number with
// identical content is stored once, and one with different content refuses the
// whole batch. The traces are compressed by the protocol codec before they
// reach the database, so what is written is the blob D-1 describes and the
// codec version beside it.
func (a *API) postLaps(w http.ResponseWriter, r *http.Request, s session) {
	id, ok := a.stintID(w, r)
	if !ok {
		return
	}
	key, ok := a.idempotencyKey(w, r)
	if !ok {
		return
	}

	var body wire.LapBatch
	fingerprintOf, err := httpx.DecodeJSONFingerprint(w, r, &body, a.bodyLimit(s.settings))
	if err != nil {
		a.invalid(w, r, err)
		return
	}
	if f := validateLapBatch(body, s.settings.Limits); f != nil {
		a.failDetail(w, r, wire.CodeInvalid, f.Message, f.Detail)
		return
	}

	rows, f := encodeLaps(body.Laps)
	if f != nil {
		a.failDetail(w, r, wire.CodeInvalid, f.Message, f.Detail)
		return
	}

	// The events this batch produced, filled in by the write and dispatched
	// after it has committed. A plugin is told about a lap that was stored,
	// never about one that was refused or replayed.
	var events []plugin.Event

	stored := a.idempotent(w, r, s, key, fingerprintOf, func(ctx context.Context, tx *db.Tx) (db.Response, error) {
		stint, err := tx.Stint(ctx, id, s.driver.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return refusal(wire.CodeNotFound, msgNoStint, map[string]any{"stint_id": id.String()})
			}
			return db.Response{}, err
		}

		// Everything that identifies the lap comes off the stint the server
		// already holds and never off the request: a client cannot file its
		// laps under another simulator, another circuit or another car by
		// sending one, because it is not asked.
		res, err := tx.AppendLaps(ctx, db.LapWrite{
			StintID:  id,
			DriverID: s.driver.ID,
			Sim:      stint.Sim,
			TrackID:  stint.TrackID,
			Car:      stint.Car,
			CarClass: stint.CarClass,
			Laps:     rows,
		})
		if err != nil {
			return db.Response{}, err
		}
		if len(res.Conflicts) > 0 {
			a.deps.Log.LogAttrs(ctx, slog.LevelWarn, "lap batch refused",
				slog.String("stint_id", id.String()),
				slog.Int("conflicts", len(res.Conflicts)))
			return refusal(wire.CodeConflict, msgLapConflict,
				map[string]any{"stint_id": id.String(), "lap_numbers": res.Conflicts})
		}

		a.deps.Log.LogAttrs(ctx, slog.LevelInfo, "laps stored",
			slog.String("stint_id", id.String()),
			slog.String("driver", s.driver.Slug),
			slog.Int("accepted", res.Accepted),
			slog.Int("sent", len(rows)))

		if a.deps.Plugins != nil {
			events = lapEvents(a.deps.Log, s, stint, body.Laps, res)
		}

		return answer(http.StatusOK, wire.LapBatchResult{
			Accepted:  res.Accepted,
			BestLapMs: res.BestLapMs,
		})
	})
	if stored {
		a.publish(r.Context(), events)
	}
}

// encodeLaps compresses every trace and fingerprints every lap, before any
// database work begins. Doing it here rather than inside the transaction keeps
// the transaction to the length of the writes: a forty-lap batch is forty zstd
// frames, and holding a connection and its locks across them would be spending
// the pool on arithmetic.
func encodeLaps(laps []wire.Lap) ([]db.LapRow, *fault) {
	out := make([]db.LapRow, 0, len(laps))
	h := hashers.Get().(hash.Hash) //nolint:errcheck // nothing but a hash.Hash is ever put in this pool.
	defer hashers.Put(h)

	for i := range laps {
		lap := &laps[i]
		corners, err := json.Marshal(cornersOrEmpty(lap.Corners))
		if err != nil {
			// Nothing in a wire.Corner can fail to encode — every field is an
			// int or a short string the validator has already bounded — so this
			// is unreachable rather than a case with a sentence for the driver.
			// It is checked because an unchecked Marshal is how an unreachable
			// case becomes a silent empty column.
			return nil, &fault{
				Message: "One of those laps carries a corner analysis this server cannot store — nothing was stored.",
				Detail:  map[string]any{"field": "corners", "index": i, "number": lap.Number, "reason": err.Error()},
			}
		}
		blob, err := trace.Encode(lap.Trace)
		if err != nil {
			// The codec refuses a trace that would not survive the round trip:
			// a channel out of range, or an offset that does not increase. The
			// driver is told the lap was dropped; the client's log gets the
			// codec's own words, which name the point and the channel.
			return nil, &fault{
				Message: "One of those laps carries a trace this server cannot store — nothing was stored.",
				Detail:  map[string]any{"field": "trace", "index": i, "number": lap.Number, "reason": err.Error()},
			}
		}
		out = append(out, db.LapRow{
			Number:     lap.Number,
			LapMs:      lap.LapMs,
			Kind:       string(lap.Kind),
			StartedAt:  lap.StartedAt,
			ContentSum: fingerprint(h, lap, blob, corners),
			TraceCodec: trace.CodecVersion,
			Trace:      blob,
			Corners:    corners,
		})
	}
	return out, nil
}

// cornersOrEmpty is the corner analysis as the column holds it: an empty array
// and never a JSON null, so that every row of the table reads the same way and
// a decoder never has to distinguish two spellings of "no corners".
func cornersOrEmpty(corners []wire.Corner) []wire.Corner {
	if corners == nil {
		return []wire.Corner{}
	}
	return corners
}

// fingerprint is what makes a repeat of lap 7 either the same lap 7 or a
// conflict. It covers every field of the lap, the compressed trace included,
// with each part length-prefixed so that no two different laps can produce the
// same byte stream by running two fields together.
//
// The trace blob is deterministic — the codec's golden vectors pin it byte for
// byte — so hashing the blob rather than the points is stable across builds and
// costs nothing, the compression having happened already.
//
// The corner analysis is in it for the same reason every other field is. It is
// not recomputable from the trace — it is measured against a reference lap the
// client chose — so two uploads of lap 7 that disagree about its corners really
// are two different laps as far as this server can tell, and answering the
// second with the first would be answering with something it did not send. The
// bytes hashed are the encoded document, which encoding/json writes
// deterministically for a slice of structs.
func fingerprint(h hash.Hash, lap *wire.Lap, blob, corners []byte) []byte {
	h.Reset()
	var num [8]byte
	put := func(v int64) {
		binary.BigEndian.PutUint64(num[:], uint64(v)) //nolint:gosec // G115: a reinterpretation of the bits into a fingerprint.
		h.Write(num[:])
	}
	put(int64(lap.Number))
	put(int64(lap.LapMs))
	put(lap.StartedAt.UTC().UnixMilli())
	put(int64(len(lap.Kind)))
	h.Write([]byte(lap.Kind))
	put(int64(len(blob)))
	h.Write(blob)
	put(int64(len(corners)))
	h.Write(corners)
	return h.Sum(nil)
}

// hashers keeps the SHA-256 states alive between batches. Forty laps is forty
// resets of one state rather than forty allocations, and a server ingesting all
// day reuses the same few.
var hashers = sync.Pool{New: func() any { return sha256.New() }}
