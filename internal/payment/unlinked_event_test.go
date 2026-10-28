package payment

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/tabloy/keygate/internal/model"
)

// A subscription event that arrives after the licence was unlinked
// must change nothing and say so, because everything the caller does
// next — the audit line, the license.canceled webhook, the dunning
// mail — describes a write that did not happen. Telling a downstream
// system a licence was cancelled while the row stays active is worse
// than silence: it revokes access the database still grants.
func TestUnlinkedSubscriptionEventIsNotApplied(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedPlan(t, s, ctx, "unlinked", "subscription")
	subID := "sub_unlinked_" + plan.Slug
	lic := &model.License{
		ProductID: plan.ProductID, PlanID: plan.ID,
		Email:      "unlinked-" + plan.Slug + "@example.com",
		LicenseKey: "KG-UNLINK-" + plan.Slug,
		Status:     model.StatusActive,
	}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create license: %v", err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", subID, lic.ID).Exec(ctx); err != nil {
		t.Fatalf("link subscription: %v", err)
	}

	// What a webhook holds: the licence as it was when the event
	// resolved it.
	inFlight, err := s.FindLicenseByStripeSubscription(ctx, subID)
	if err != nil {
		t.Fatalf("find by subscription: %v", err)
	}

	// The admin unlinks in the window before the write.
	if cleared, err := s.ClearStripeSubscription(ctx, lic.ID, subID); err != nil || !cleared {
		t.Fatalf("unlink = (%v, %v), want (true, nil)", cleared, err)
	}

	h := &StripeHandler{Store: s}
	canceled := time.Now()
	inFlight.Status = model.StatusCanceled
	inFlight.CanceledAt = &canceled
	if landed, err := h.applyLicenseFromSubscription(ctx, inFlight, "customer.subscription.deleted", nil, "status", "canceled_at"); landed || err != nil {
		t.Fatalf("stale event = (%v, %v), want (false, nil); the caller would then audit and dispatch a cancellation that never happened, or have Stripe retry it", landed, err)
	}
	if landed, err := h.endLicenseFromSubscription(ctx, inFlight, "status", "canceled_at"); landed || err != nil {
		t.Fatalf("deletion of an unlinked licence = (%v, %v), want (false, nil)", landed, err)
	}

	after, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != model.StatusActive {
		t.Errorf("status = %q, want %q", after.Status, model.StatusActive)
	}
	if after.CanceledAt != nil {
		t.Errorf("canceled_at = %v, want nil", after.CanceledAt)
	}

	// A licence still on its subscription is written as before.
	lic2 := &model.License{
		ProductID: plan.ProductID, PlanID: plan.ID,
		Email:      "linked-" + plan.Slug + "@example.com",
		LicenseKey: "KG-LINKED-" + plan.Slug,
		Status:     model.StatusActive,
	}
	if err := s.CreateLicense(ctx, lic2); err != nil {
		t.Fatalf("create second license: %v", err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", subID+"-live", lic2.ID).Exec(ctx); err != nil {
		t.Fatalf("link second subscription: %v", err)
	}
	live, err := s.FindLicenseByStripeSubscription(ctx, subID+"-live")
	if err != nil {
		t.Fatalf("find second: %v", err)
	}
	live.Status = model.StatusCanceled
	if landed, err := h.applyLicenseFromSubscription(ctx, live, "customer.subscription.deleted", nil, "status"); !landed {
		t.Fatalf("a licence still on its subscription was not written: %v", err)
	}
	if reloaded, err := s.FindLicenseByID(ctx, lic2.ID); err != nil || reloaded.Status != model.StatusCanceled {
		t.Fatalf("second license status = %v (err %v), want canceled", reloaded.Status, err)
	}
}

// Every caller of applyLicenseFromSubscription has to act on its
// answer. Ignoring it is how the side effects got out of step with the
// database in the first place, and the compiler will not say a word
// about a dropped bool.
func TestEveryCallerChecksTheApplyResult(t *testing.T) {
	src, err := os.ReadFile("stripe.go")
	if err != nil {
		t.Fatalf("read stripe.go: %v", err)
	}
	all := regexp.MustCompile(`h\.(?:applyLicenseFromSubscription|endLicenseFromSubscription)\(`).FindAllString(string(src), -1)
	if len(all) == 0 {
		t.Fatal("no call sites found — did the helper get renamed?")
	}
	// Each one stops on false and hands the error to the webhook, which
	// has Stripe retry a write that failed.
	checked := regexp.MustCompile(`(?m)^\s*if landed, err := h\.(?:applyLicenseFromSubscription|endLicenseFromSubscription)\(.*\); !landed \{\n\s*return err\n`).FindAllString(string(src), -1)
	if len(checked) != len(all) {
		t.Errorf("%d of %d call sites read `if landed, err := h.applyLicenseFromSubscription(...); !landed { return err }`", len(checked), len(all))
	}
	if len(all) < 4 {
		t.Errorf("found %d call sites, expected four: invoice.paid, the subscription state, the deletion and payment_failed", len(all))
	}
}

// A licence write that fails (the database, not a licence refused as
// unlinked, stale or restricted) fails the event when the write came
// from a current read of the subscription, or ends it, so Stripe sends
// it again and nothing announces a change that was not made. A write
// from the payload alone is answered 200: a late retry of it could undo
// a newer state (see applyLicenseFromSubscription).
func TestFailedLicenseWriteIsRetried(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	plan := seedPlan(t, s, ctx, "failwrite", "subscription")
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	subID := "sub_failwrite_" + suffix
	lic := &model.License{ProductID: plan.ProductID, PlanID: plan.ID, Email: "failwrite-" + suffix + "@example.com",
		LicenseKey: "KG-FAILWRITE-" + suffix, Status: model.StatusActive, StripeSubscriptionID: subID}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	fn := "fail_license_" + suffix
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'database unavailable'; END $$`, fn)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON licenses FOR EACH ROW
		WHEN (NEW.id = '%s') EXECUTE FUNCTION %s()`, fn, lic.ID, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON licenses", fn))
		_, _ = s.DB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})

	h := &StripeHandler{Store: s}
	secret := "whsec_failwrite_" + suffix
	h.SetWebhookSecret(secret)
	sub := map[string]any{"id": subID, "object": "subscription"}
	inv := map[string]any{"object": "invoice", "subscription": subID}
	type event struct {
		typ     string
		obj     map[string]any
		retried bool
	}
	send := func(phase string, events []event) {
		t.Helper()
		for _, ev := range events {
			id := "evt_failwrite_" + phase + "_" + ev.typ + "_" + suffix
			code, out := signedWebhook(t, h, secret, id, ev.typ, ev.obj)
			claimed := s.IsEventProcessed(ctx, processedEventClaimProvider, id)
			done := s.IsEventProcessed(ctx, processedEventDoneProvider, id)
			switch {
			case ev.retried && (code != http.StatusInternalServerError || out["retry"] != true):
				t.Errorf("%s %s: a failed licence write was answered %d %v; want 500 so Stripe sends it again", phase, ev.typ, code, out)
			case ev.retried && (claimed || done):
				t.Errorf("%s %s: event claim not released", phase, ev.typ)
			case !ev.retried && code != http.StatusOK:
				t.Errorf("%s %s: a failed payload write was answered %d %v; want 200, a late retry could undo a newer state", phase, ev.typ, code, out)
			case !ev.retried && (claimed || !done):
				t.Errorf("%s %s: event not recorded as done (claim %v, done %v)", phase, ev.typ, claimed, done)
			}
		}
	}

	// No API key: every handler but the deletion writes its payload.
	prevKey := stripe.Key
	t.Cleanup(func() { stripe.Key = prevKey })
	stripe.Key = ""
	send("payload", []event{
		{"invoice.paid", inv, false},
		{"customer.subscription.updated", map[string]any{"id": subID, "object": "subscription", "status": "past_due"}, false},
		{"customer.subscription.deleted", sub, true},
		{"invoice.payment_failed", inv, false},
		{"customer.subscription.paused", sub, false},
		{"customer.subscription.resumed", sub, false},
	})

	// Stripe answers: invoice.paid and updated write what it says now,
	// with the seats when it is live, without them once it ended.
	var stripeSays atomic.Value
	stripeSays.Store("past_due")
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":%q,"cancel_at_period_end":false,"items":{"object":"list","data":[{"id":"si_1","current_period_end":%d}]}}`,
			subID, stripeSays.Load(), time.Now().Add(30*24*time.Hour).Unix())
	})
	send("current", []event{
		{"invoice.paid", inv, true},
		{"customer.subscription.updated", sub, true},
	})
	stripeSays.Store("canceled")
	send("ended", []event{
		{"customer.subscription.updated", sub, true},
	})

	status := func() string {
		t.Helper()
		got, err := s.FindLicenseByID(ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	if st := status(); st != model.StatusActive {
		t.Fatalf("licence status = %s, want active: every write failed", st)
	}
	time.Sleep(200 * time.Millisecond) // audit lines are written in the background
	var audits int
	if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ?", lic.ID).Scan(ctx, &audits); err != nil || audits != 0 {
		t.Fatalf("audit lines = %d (err %v), want none for a licence that was never written", audits, err)
	}

	// The control: without the failing trigger the same reads are written
	// and answered 200, so the 500s above came from the write.
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf("DROP TRIGGER %s ON licenses", fn)); err != nil {
		t.Fatal(err)
	}
	stripeSays.Store("past_due")
	if code, out := signedWebhook(t, h, secret, "evt_failwrite_control_paid_"+suffix, "invoice.paid", inv); code != http.StatusOK || status() != model.StatusPastDue {
		t.Fatalf("control invoice.paid = %d %v, licence %s; want 200 and past_due written", code, out, status())
	}
	stripeSays.Store("canceled")
	if code, out := signedWebhook(t, h, secret, "evt_failwrite_control_ended_"+suffix, "customer.subscription.updated", sub); code != http.StatusOK || status() != model.StatusCanceled {
		t.Fatalf("control updated = %d %v, licence %s; want 200 and canceled written", code, out, status())
	}
}

// A deletion is the last event a subscription sends, so a lookup that
// failed is retried; only a subscription that is not ours is answered
// as done.
func TestSubscriptionDeletedRetriesAFailedLookup(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	raw := []byte(`{"id":"sub_deleted_lookup_` + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") + `"}`)

	if err := h.onSubscriptionDeleted(ctx, raw); err != nil {
		t.Fatalf("unknown subscription: %v, want nil", err)
	}
	failing, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.onSubscriptionDeleted(failing, raw); err == nil {
		t.Fatal("a failed lookup was answered as done; Stripe would never send the deletion again")
	}
}
