package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// The audit actions this server writes. They are constants rather than strings
// at the call site so that a query for "who changed the settings" is a query
// for a value and not a guess at the spelling somebody used.
const (
	// ActionSettingsIdentity is a change to what discovery publishes about
	// the organisation.
	ActionSettingsIdentity = "settings.identity"
	// ActionSettingsVoice is a change to the voice service. Nothing writes it
	// any more — the voice left the core with its vendor — and it is kept
	// because an operator's audit log still holds rows that say it, and a
	// reader that did not know the name would show them as nothing.
	ActionSettingsVoice = "settings.voice"
	// ActionSettingsCoaching is a change to the language-model settings.
	ActionSettingsCoaching = "settings.coaching"
	// ActionSettingsLimits is a change to the published limits.
	ActionSettingsLimits = "settings.limits"
	// ActionSettingsKeyRemoved is an operator clearing a stored API key. Like
	// [ActionSettingsVoice] nothing writes it now: the core stores no
	// credential, and clearing a plugin's is recorded against the plugin.
	ActionSettingsKeyRemoved = "settings.key_removed"
	// ActionVoiceTested is the Test voice button.
	ActionVoiceTested = "settings.voice_tested"
	// ActionDataKeyRegenerated is the data key being replaced.
	ActionDataKeyRegenerated = "danger.data_key_regenerated"
	// ActionDevicesRevoked is every device token being revoked at once.
	ActionDevicesRevoked = "danger.devices_revoked"
	// ActionDeviceRevoked is one machine being signed out from the devices
	// page.
	ActionDeviceRevoked = "devices.revoked"
	// ActionDriverDevicesRevoked is every machine of one driver being signed
	// out at once — the lost-laptop action.
	ActionDriverDevicesRevoked = "devices.revoked_for_driver"
	// ActionRetentionChanged is a change to how long traces are kept.
	ActionRetentionChanged = "data.retention"
	// ActionTracesPruned is a retention prune being started.
	ActionTracesPruned = "data.traces_pruned"
	// ActionClientBuilt is one Windows client being built for an address.
	ActionClientBuilt = "clients.built"
	// ActionCertificateStored is an operator giving this server a code-signing
	// certificate, and ActionCertificateRemoved is taking it away again. The
	// subject is the certificate's own subject, which is public: it is printed
	// inside every client signed with it.
	ActionCertificateStored  = "clients.certificate_stored"
	ActionCertificateRemoved = "clients.certificate_removed"
	// ActionDriverSignedOut is a driver's browser session being ended by the
	// operator. It is separate from revoking their machines: a machine's token
	// uploads laps and a browser session reads them, and ending one is not
	// ending the other.
	ActionDriverSignedOut = "drivers.signed_out"
)

// AuditEntry is one line of the trail: who did what, to what, and when.
//
// Detail names the fields that changed and never carries their values. A
// settings change can be a change to an API key, and a trail that recorded the
// value would be the one place the key was readable.
type AuditEntry struct {
	// ID is the row's identity, and At is when it was written.
	ID int64
	At time.Time
	// Actor is the administrator's email address.
	Actor string
	// Action is one of the constants above.
	Action string
	// Subject is what was acted on — a settings group, a count of devices.
	Subject string
	// Detail is the JSON document, already encoded.
	Detail []byte
}

// Fields reads the field names out of [AuditEntry.Detail]. A detail that does
// not carry any is no fields rather than an error: the trail is written by
// several versions of this server over its life.
func (e AuditEntry) Fields() []string {
	var d struct {
		Changed []string `json:"changed"`
	}
	if err := json.Unmarshal(e.Detail, &d); err != nil {
		return nil
	}
	return d.Changed
}

// WriteAudit records one change. The caller passes the names of what changed,
// never the values.
func (s *Store) WriteAudit(ctx context.Context, actor, action, subject string, changed []string) error {
	if changed == nil {
		changed = []string{}
	}
	detail, err := json.Marshal(map[string]any{"changed": changed})
	if err != nil {
		return fmt.Errorf("db: cannot encode the audit detail: %w", err)
	}
	if _, err := s.q.InsertAuditEntry(ctx, gen.InsertAuditEntryParams{
		Actor:   actor,
		Action:  action,
		Subject: subject,
		Detail:  detail,
	}); err != nil {
		return fmt.Errorf("db: cannot record what changed: %w", err)
	}
	return nil
}

// RecentAudit reads the newest entries, for the panel and for the tests that
// assert a change was recorded.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.q.RecentAuditEntries(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the audit trail: %w", err)
	}
	return auditEntries(rows), nil
}

// AuditFor reads the newest entries for one action.
func (s *Store) AuditFor(ctx context.Context, action string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.q.AuditEntriesForAction(ctx, gen.AuditEntriesForActionParams{
		Action: action,
		Limit:  int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("db: cannot read the audit trail: %w", err)
	}
	return auditEntries(rows), nil
}

func auditEntries(rows []gen.AuditLog) []AuditEntry {
	out := make([]AuditEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, AuditEntry{
			ID:      r.ID,
			At:      r.At.Time,
			Actor:   r.Actor,
			Action:  r.Action,
			Subject: r.Subject,
			Detail:  r.Detail,
		})
	}
	return out
}
