package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// A licence already past due keeps the moment its episode began, whatever
// later write enters "past_due" again: the dunning ladder and the
// once-per-episode notice key on it. A licence entering past_due afresh
// takes the new moment.
func TestPastDueKeepsTheEpisodeStart(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	began := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusPastDue, nil)
	sub := "sub_episode_" + lic.ID
	linkSubscription(t, s, ctx, lic.ID, sub)
	if _, err := s.DB.NewRaw("UPDATE licenses SET past_due_at = ? WHERE id = ?", began, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	pastDue := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := s.DB.NewRaw("SELECT past_due_at FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	now := time.Now()
	write := &model.License{ID: lic.ID, StripeSubscriptionID: sub, Status: model.StatusPastDue, PastDueAt: &now}
	if err := s.UpdateLicenseFromSubscription(ctx, write, "status", "past_due_at"); err != nil {
		t.Fatal(err)
	}
	if got := pastDue(); got == nil || !got.Equal(began) {
		t.Fatalf("past_due_at = %v, want the episode's start %v kept", got, began)
	}
	if write.PastDueAt == nil || !write.PastDueAt.Equal(began) {
		t.Fatalf("the caller's copy says %v, want %v, as written", write.PastDueAt, began)
	}

	fresh := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	fsub := "sub_episode_" + fresh.ID
	linkSubscription(t, s, ctx, fresh.ID, fsub)
	if err := s.UpdateLicenseFromSubscription(ctx, &model.License{ID: fresh.ID, StripeSubscriptionID: fsub, Status: model.StatusPastDue, PastDueAt: &now}, "status", "past_due_at"); err != nil {
		t.Fatal(err)
	}
	var at *time.Time
	if err := s.DB.NewRaw("SELECT past_due_at FROM licenses WHERE id = ?", fresh.ID).Scan(ctx, &at); err != nil || at == nil || at.Sub(now).Abs() > time.Second {
		t.Fatalf("a licence entering past_due: past_due_at = %v (err %v), want %v", at, err, now)
	}
}
