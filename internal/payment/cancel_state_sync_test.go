package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// SyncCancelStates asks Stripe about subscriptions whose cancel state was
// never recorded and stores the answer: set to cancel at period end,
// renewing, and a licence with no subscription row. A 404 (this key
// cannot see it) stays unknown and is asked about again next run.
func TestSyncCancelStates(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Sync", Slug: "sync-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "sync-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, withRow bool, offset time.Duration) *model.License {
		t.Helper()
		until := time.Now().Add(10*24*time.Hour + offset)
		lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: name + "-" + suffix + "@example.com",
			LicenseKey: "KEY-sync-" + name + "-" + suffix, Status: model.StatusActive, ValidUntil: &until,
			StripeSubscriptionID: "sub_sync_" + name + "_" + suffix}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatal(err)
		}
		if withRow {
			if err := store.SyncLicenseSubscriptionIn(ctx, s.DB, lic.ID, plan, model.StatusActive, &until); err != nil {
				t.Fatal(err)
			}
		}
		return lic
	}
	canceling := mk("canceling", true, 0)
	renewing := mk("renewing", true, time.Hour)
	deleted := mk("deleted", true, 2*time.Hour)
	noRow := mk("norow", false, 3*time.Hour)
	// The unseen one would otherwise stay listed for the dev server's own
	// sync to keep asking Stripe about. A defer, not t.Cleanup: it must
	// run before the deferred s.Close.
	defer func() {
		_, _ = s.DB.NewRaw("UPDATE licenses SET status = 'expired' WHERE id = ?", deleted.ID).Exec(context.Background())
	}()

	periodEnd := time.Now().Add(20 * 24 * time.Hour).Unix()
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")
		if r.Method != http.MethodGet || id == r.URL.Path {
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		item := fmt.Sprintf(`{"object":"list","data":[{"id":"si_1","current_period_end":%d}]}`, periodEnd)
		switch id {
		case canceling.StripeSubscriptionID:
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":true,"cancel_at":%d,"items":%s}`, id, periodEnd, item)
		case renewing.StripeSubscriptionID, noRow.StripeSubscriptionID:
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":false,"items":%s}`, id, item)
		case deleted.StripeSubscriptionID:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such subscription"}}`)
		default:
			t.Errorf("lookup of a subscription outside the fixture: %s", id)
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"no"}}`)
		}
	})

	fixtures := []*model.License{canceling, renewing, deleted, noRow}
	inList := func(lic *model.License) bool {
		t.Helper()
		targets, err := s.FindStripeCancelStatesToSync(ctx, nil, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		for _, tg := range targets {
			if tg.LicenseID == lic.ID && tg.SubscriptionID == lic.StripeSubscriptionID {
				return true
			}
		}
		return false
	}
	for _, lic := range fixtures {
		if !inList(lic) {
			t.Fatalf("%s: not listed for sync", lic.Email)
		}
	}

	// Run the per-subscription step on the fixtures only: the listing
	// spans the whole (shared) database.
	h := &StripeHandler{Store: s}
	for _, lic := range fixtures {
		if !h.syncCancelState(ctx, store.StripeCancelStateTarget{LicenseID: lic.ID, SubscriptionID: lic.StripeSubscriptionID}) {
			t.Fatalf("%s: sync did not move past it", lic.Email)
		}
	}

	state := func(lic *model.License) string {
		t.Helper()
		var v *bool
		err := s.DB.NewRaw(`SELECT cancel_at_period_end FROM subscriptions
			WHERE license_id = ? AND cancel_state_synced_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, lic.ID).Scan(context.Background(), &v)
		if err != nil || v == nil {
			return "unknown"
		}
		if *v {
			return "ends"
		}
		return "renews"
	}
	for lic, want := range map[*model.License]string{canceling: "ends", renewing: "renews", deleted: "unknown", noRow: "renews"} {
		if got := state(lic); got != want {
			t.Errorf("%s: state %s, want %s", lic.Email, got, want)
		}
	}

	// Synced subscriptions are not listed again; the unseen one is.
	for _, lic := range fixtures {
		if got, want := inList(lic), lic == deleted; got != want {
			t.Errorf("%s: listed after sync = %v, want %v", lic.Email, got, want)
		}
	}

	// A webhook recorded a newer state after the sync read Stripe (the
	// customer resumed): the sync's older "ends" must not overwrite it.
	asOf, err := s.StripeReadStamp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscriptionCancelScheduled(ctx, canceling.ID, false, asOf); err != nil {
		t.Fatal(err)
	}
	h.syncCancelState(ctx, store.StripeCancelStateTarget{LicenseID: canceling.ID, SubscriptionID: canceling.StripeSubscriptionID})
	if got := state(canceling); got != "renews" {
		t.Fatalf("sync overwrote the webhook's newer state: %s", got)
	}

	// The walk continues after a given target: the unseen one is not
	// listed again after itself, so it cannot hold the head of every
	// batch.
	all, err := s.FindStripeCancelStatesToSync(ctx, nil, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var cursor *store.StripeCancelStateTarget
	var behind []string // listed after the cursor on this walk
	for i := range all {
		switch {
		case all[i].LicenseID == deleted.ID:
			cursor = &all[i]
		case cursor != nil:
			behind = append(behind, all[i].LicenseID)
		}
	}
	if cursor == nil {
		t.Fatal("unseen subscription not listed")
	}
	listedAfter := func() map[string]bool {
		t.Helper()
		after, err := s.FindStripeCancelStatesToSync(ctx, cursor, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, tg := range after {
			got[tg.LicenseID] = true
		}
		return got
	}
	if listedAfter()[deleted.ID] {
		t.Fatal("cursor did not move past the unseen subscription")
	}

	// The cursor keeps its place when that licence's own date moves
	// later in the meantime: nothing that was behind it is skipped.
	if _, err := s.DB.NewRaw("UPDATE licenses SET valid_until = valid_until + interval '400 days' WHERE id = ?", deleted.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	got := listedAfter()
	for _, id := range behind {
		if !got[id] {
			t.Fatalf("licence %s behind the cursor was skipped once the cursor licence's date moved", id)
		}
	}
}

// A subscription.updated webhook records the cancel state as known, from
// the event itself; resuming clears it again.
func TestSubscriptionUpdatedRecordsCancelState(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Hook", Slug: "hook-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "hook-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(20 * 24 * time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "hook-" + suffix + "@example.com",
		LicenseKey: "KEY-hook-" + suffix, Status: model.StatusActive, ValidUntil: &until, StripeSubscriptionID: "sub_hook_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncLicenseSubscriptionIn(ctx, s.DB, lic.ID, plan, model.StatusActive, &until); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}
	// Stripe answers with the state the event carries.
	var now bool
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":%t,"items":{"object":"list","data":[{"id":"si_1","current_period_end":%d}]}}`,
			lic.StripeSubscriptionID, now, until.Unix())
	})
	send := func(cancelAtPeriodEnd bool) {
		t.Helper()
		now = cancelAtPeriodEnd
		raw := fmt.Sprintf(`{"id":%q,"status":"active","cancel_at_period_end":%t,"current_period_end":%d}`,
			lic.StripeSubscriptionID, cancelAtPeriodEnd, until.Unix())
		if err := h.onSubscriptionUpdated(ctx, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (synced bool, ends bool) {
		t.Helper()
		if err := s.DB.NewRaw(`SELECT cancel_state_synced_at IS NOT NULL, cancel_at_period_end FROM subscriptions
			WHERE license_id = ? ORDER BY created_at DESC LIMIT 1`, lic.ID).Scan(ctx, &synced, &ends); err != nil {
			t.Fatal(err)
		}
		return synced, ends
	}
	send(true)
	if synced, ends := read(); !synced || !ends {
		t.Fatalf("after cancel: synced=%v ends=%v", synced, ends)
	}
	send(false)
	if synced, ends := read(); !synced || ends {
		t.Fatalf("after resume: synced=%v ends=%v", synced, ends)
	}
	// Not one of ours: handled, nothing to retry.
	if err := h.onSubscriptionUpdated(ctx, []byte(`{"id":"sub_unknown_`+suffix+`","status":"active"}`)); err != nil {
		t.Fatalf("unknown subscription returned %v, want nil", err)
	}
}

// Stripe delivers events out of order. A stale "cancel at period end"
// arriving after the customer resumed must not win: the handler acts on
// the subscription as Stripe has it now. A failed lookup is retried; on a
// 404 (this key cannot see it) the payload is not recorded as the state.
func TestSubscriptionUpdatedUsesCurrentState(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Order", Slug: "order-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "order-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "order-" + suffix + "@example.com",
		LicenseKey: "KEY-order-" + suffix, Status: model.StatusActive, ValidUntil: &until, StripeSubscriptionID: "sub_order_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncLicenseSubscriptionIn(ctx, s.DB, lic.ID, plan, model.StatusActive, &until); err != nil {
		t.Fatal(err)
	}

	// What Stripe answers for the subscription right now.
	var reply func(w http.ResponseWriter)
	calls := 0
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions/"+lic.StripeSubscriptionID {
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls++
		reply(w)
	})
	current := func(cancelAtPeriodEnd bool) func(w http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":%t,"items":{"object":"list","data":[{"id":"si_1","current_period_end":%d}]}}`,
				lic.StripeSubscriptionID, cancelAtPeriodEnd, until.Unix())
		}
	}
	event := func(cancelAtPeriodEnd bool) []byte {
		return []byte(fmt.Sprintf(`{"id":%q,"status":"active","cancel_at_period_end":%t,"current_period_end":%d}`,
			lic.StripeSubscriptionID, cancelAtPeriodEnd, until.Unix()))
	}
	ends := func() *bool {
		t.Helper()
		var v *bool
		if err := s.DB.NewRaw(`SELECT cancel_at_period_end FROM subscriptions
			WHERE license_id = ? AND cancel_state_synced_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, lic.ID).Scan(ctx, &v); err != nil {
			return nil
		}
		return v
	}
	h := &StripeHandler{Store: s}

	// A stale "cancel" after the customer resumed: Stripe says it renews.
	reply = current(false)
	if err := h.onSubscriptionUpdated(ctx, event(true)); err != nil {
		t.Fatal(err)
	}
	if v := ends(); v == nil || *v {
		t.Fatalf("stale cancel event won over the current state: %v", v)
	}

	// Stripe unreachable: an error, so the webhook is retried, and
	// nothing recorded from the payload.
	reply = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"type":"api_error","message":"boom"}}`)
	}
	if err := h.onSubscriptionUpdated(ctx, event(true)); err == nil {
		t.Fatal("lookup failure must return an error for a retry")
	}
	if v := ends(); v == nil || *v {
		t.Fatalf("state changed on a failed lookup: %v", v)
	}

	// Not visible to this key: the payload is history and must not
	// overwrite the recorded state (a stale "cancel" after a resume).
	reply = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such subscription"}}`)
	}
	if err := h.onSubscriptionUpdated(ctx, event(true)); err != nil {
		t.Fatal(err)
	}
	if v := ends(); v == nil || *v {
		t.Fatalf("404 fallback overwrote the recorded state: %v", v)
	}
	if calls == 0 {
		t.Fatal("handler never asked Stripe")
	}
}

// A subscription checkout leaves the new subscription's cancel state to
// the background sync, which reads Stripe once the licence exists — a
// copy read during fulfilment could predate a cancellation whose webhook
// found no licence yet. The sync then records it.
func TestFulfillCheckout_LeavesCancelStateToSync(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "New sub", Slug: "newsub-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	price := "price_newsub_" + suffix
	plan := &model.Plan{ProductID: prod.ID, Name: "Monthly", Slug: "newsub-" + suffix, LicenseType: "subscription",
		LicenseModel: "standard", BillingInterval: "month", StripePriceID: price}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	subID := "sub_newsub_" + suffix
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()
	// Set to cancel by the time the sync asks; fulfilment only needs the
	// price.
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions/"+subID {
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":true,
			"items":{"object":"list","data":[{"id":"si_1","current_period_end":%d,"price":{"id":%q,"object":"price"}}]}}`,
			subID, periodEnd, price)
	})

	h := &StripeHandler{Store: s}
	email := "newsub-" + suffix + "@example.com"
	fulfilOK(h, ctx, email, "", subID, "", map[string]string{"session_id": "cs_test_newsub_" + suffix}, "test")

	lic, err := s.FindLicenseByStripeSubscription(ctx, subID)
	if err != nil {
		t.Fatalf("license not created: %v", err)
	}
	var synced bool
	if err := s.DB.NewRaw(`SELECT EXISTS (SELECT 1 FROM subscriptions
		WHERE license_id = ? AND cancel_state_synced_at IS NOT NULL)`, lic.ID).Scan(ctx, &synced); err != nil {
		t.Fatal(err)
	}
	if synced {
		t.Fatal("fulfilment recorded a cancel state; it is the sync's to record")
	}

	if !h.syncCancelState(ctx, store.StripeCancelStateTarget{LicenseID: lic.ID, SubscriptionID: subID}) {
		t.Fatal("sync did not finish")
	}
	var ends bool
	if err := s.DB.NewRaw(`SELECT cancel_at_period_end FROM subscriptions
		WHERE license_id = ? AND cancel_state_synced_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, lic.ID).Scan(ctx, &ends); err != nil {
		t.Fatal(err)
	}
	if !ends {
		t.Fatal("sync did not record the cancellation")
	}
}

// Deletion is final: a subscription.updated whose read started before it
// (and so may still have seen "active") cannot bring the licence back.
func TestSubscriptionDeletedOutranksOlderReads(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Deleted", Slug: "deleted-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "deleted-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(20 * 24 * time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "deleted-" + suffix + "@example.com",
		LicenseKey: "KEY-deleted-" + suffix, Status: model.StatusActive, ValidUntil: &until, StripeSubscriptionID: "sub_deleted_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	h := &StripeHandler{Store: s}

	before, err := s.StripeReadStamp(ctx) // an updated webhook asks Stripe...
	if err != nil {
		t.Fatal(err)
	}
	h.onSubscriptionDeleted(ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID))) // ...the deletion lands
	stale := *lic
	stale.Status = model.StatusActive // what that earlier read saw
	if err := s.UpdateLicenseFromSubscriptionRead(ctx, &stale, before, "status"); !errors.Is(err, store.ErrStaleSubscriptionRead) {
		t.Fatalf("read from before the deletion: err=%v, want ErrStaleSubscriptionRead", err)
	}
	var status string
	if err := s.DB.NewRaw("SELECT status FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &status); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusCanceled {
		t.Fatalf("licence brought back after deletion: %s", status)
	}
}

// A subscription.updated that read Stripe after the deletion may write
// "canceled" first, and tells the end itself; the deletion must still
// apply, must not tell it a second time, and must not move the read
// stamp back.
func TestSubscriptionDeletedAppliesAfterANewerRead(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Deleted late", Slug: "deleted-late-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "deleted-late-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(20 * 24 * time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "deleted-late-" + suffix + "@example.com",
		LicenseKey: "KEY-deleted-late-" + suffix, Status: model.StatusActive, ValidUntil: &until, StripeSubscriptionID: "sub_deleted_late_" + suffix}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	// The newer read already applied "canceled", stamped after the moment
	// the deletion handler will take.
	newer := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	h := &StripeHandler{Store: s}
	canceled := *lic
	if err := h.applySubscriptionState(ctx, &canceled, &subscriptionEvent{ID: lic.StripeSubscriptionID, Status: "canceled"}, &newer, "customer.subscription.updated"); err != nil {
		t.Fatal(err)
	}
	if err := h.onSubscriptionDeleted(ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID))); err != nil {
		t.Fatal(err)
	}

	// Audit lines are written in the background.
	var audits int
	for i := 0; i < 30 && audits == 0; i++ {
		if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'canceled'", lic.ID).Scan(ctx, &audits); err != nil {
			t.Fatal(err)
		}
		if audits == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond) // and no second one arrives
	if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'canceled'", lic.ID).Scan(ctx, &audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("deletion after a newer read: the end told %d times, want once", audits)
	}
	var status string
	var stamp time.Time
	var canceledAt *time.Time
	if err := s.DB.NewRaw("SELECT status, stripe_synced_at, canceled_at FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &status, &stamp, &canceledAt); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusCanceled || canceledAt == nil {
		t.Fatalf("deletion not applied: status=%s canceled_at=%v", status, canceledAt)
	}
	if !stamp.Equal(newer) {
		t.Fatalf("read stamp moved back to %v, want %v", stamp, newer)
	}
}
