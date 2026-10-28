package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A pause Stripe already made, read again, is written again: it keeps the
// pause's start and changes nothing else, but records the read, so a
// resume read before it (and written after) is refused as stale. An
// operator's suspension still refuses the pause.
func TestRepeatedStripePauseRecordsTheRead(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	began := time.Now().Add(-time.Hour).Truncate(time.Second)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusSuspended, nil)
	sub := "sub_repause_" + lic.ID
	linkSubscription(t, s, ctx, lic.ID, sub)
	if _, err := s.DB.NewRaw("UPDATE licenses SET suspended_by = 'stripe', suspended_at = ? WHERE id = ?", began, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	state := func(id string) (string, *time.Time) {
		t.Helper()
		var st string
		var at *time.Time
		if err := s.DB.NewRaw("SELECT status || ':' || COALESCE(suspended_by, ''), suspended_at FROM licenses WHERE id = ?", id).Scan(ctx, &st, &at); err != nil {
			t.Fatal(err)
		}
		return st, at
	}
	stamp := func() time.Time {
		t.Helper()
		at, err := s.StripeReadStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}

	readResume := stamp() // request A reads "active"
	readPause := stamp()  // Stripe pauses again; request B reads "paused"
	now := time.Now()
	pause := &model.License{ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusSuspended,
		SuspendedAt: &now, SuspendedBy: model.SuspendedByStripe}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, pause, readPause, "status", "suspended_at", "suspended_by"); err != nil {
		t.Fatalf("repeated pause: %v, want it written", err)
	}
	if !pause.WasStripePaused {
		t.Fatal("the write did not see the pause it repeats")
	}
	resume := &model.License{ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusActive}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, resume, readResume, "status", "suspended_at", "suspended_by"); !errors.Is(err, store.ErrStaleSubscriptionRead) {
		t.Fatalf("older resume read: %v, want refused as stale", err)
	}
	if st, at := state(lic.ID); st != "suspended:stripe" || at == nil || !at.Equal(began) {
		t.Fatalf("after the race: %s since %v, want suspended:stripe since %v", st, at, began)
	}

	op := newLicenseOn(t, s, ctx, "subscription", model.StatusSuspended, nil)
	osub := "sub_repause_" + op.ID
	linkSubscription(t, s, ctx, op.ID, osub)
	if _, err := s.DB.NewRaw("UPDATE licenses SET suspended_by = 'admin', suspended_at = now() WHERE id = ?", op.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	opPause := &model.License{ID: op.ID, StripeSubscriptionID: osub, Status: model.StatusSuspended, SuspendedAt: &now, SuspendedBy: model.SuspendedByStripe}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, opPause, stamp(), "status", "suspended_at", "suspended_by"); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("pause on an operator's suspension: %v, want refused", err)
	}
	if st, _ := state(op.ID); st != "suspended:admin" {
		t.Fatalf("operator's suspension became %s", st)
	}

	// Nor does a pause reach a licence whose subscription ended.
	ended := newLicenseOn(t, s, ctx, "subscription", model.StatusCanceled, nil)
	esub := "sub_repause_" + ended.ID
	linkSubscription(t, s, ctx, ended.ID, esub)
	endPause := &model.License{ID: ended.ID, StripeSubscriptionID: esub, Status: model.StatusSuspended, SuspendedAt: &now, SuspendedBy: model.SuspendedByStripe}
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, endPause, stamp(), "status", "suspended_at", "suspended_by"); !errors.Is(err, store.ErrLicenseRestricted) {
		t.Fatalf("pause on a canceled licence: %v, want refused", err)
	}
	if st, _ := state(ended.ID); st != "canceled:" {
		t.Fatalf("canceled licence became %s", st)
	}
}

// A write that leaves the status alone leaves the subscription row's
// status alone too: the caller's copy may say "trialing" from before a
// resume, and the row must keep what the licence has.
func TestDateOnlyWriteKeepsTheSubscriptionStatus(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	sub := "sub_dateonly_" + lic.ID
	linkSubscription(t, s, ctx, lic.ID, sub)
	if err := s.CreateSubscription(ctx, &model.Subscription{LicenseID: lic.ID, PlanID: lic.PlanID, Status: model.StatusActive}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(30 * 24 * time.Hour)
	stale := &model.License{ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusTrialing, ValidUntil: &until}
	if err := s.UpdateLicenseFromSubscription(ctx, stale, "valid_until"); err != nil {
		t.Fatal(err)
	}
	var licStatus, subStatus string
	if err := s.DB.NewRaw("SELECT l.status, s.status FROM licenses l JOIN subscriptions s ON s.license_id = l.id WHERE l.id = ?", lic.ID).Scan(ctx, &licStatus, &subStatus); err != nil {
		t.Fatal(err)
	}
	if licStatus != model.StatusActive || subStatus != model.StatusActive {
		t.Fatalf("after a date-only write: licence %s, subscription %s, want both active", licStatus, subStatus)
	}
}

// The terms write finds a licence that is gone unlinked, like every
// other subscription write: the event is done, not retried.
func TestTermsWriteOnAGoneLicenceIsUnlinked(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	gone := &model.License{ID: "00000000-0000-0000-0000-000000000000", StripeSubscriptionID: "sub_gone", Status: model.StatusActive, Quantity: 1}
	if _, _, err := s.UpdateLicenseFromSubscriptionReadTerms(ctx, gone, time.Now(), "status", "quantity"); !errors.Is(err, store.ErrSubscriptionUnlinked) {
		t.Fatalf("terms write on a gone licence: %v, want ErrSubscriptionUnlinked", err)
	}
}
