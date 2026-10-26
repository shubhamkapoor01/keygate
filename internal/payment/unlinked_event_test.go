package payment

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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
	calls := regexp.MustCompile(`(?m)^(\s*)(.*)h\.(?:applyLicenseFromSubscription|endLicenseFromSubscription)\(`).FindAllStringSubmatch(string(src), -1)
	if len(calls) == 0 {
		t.Fatal("no call sites found — did the helper get renamed?")
	}
	for _, c := range calls {
		if prefix := c[2]; prefix != "if landed, err := " {
			t.Errorf("call site ignores the result (prefix %q); it must read `if landed, err := h.applyLicenseFromSubscription(...); !landed { return err }`", prefix)
		}
	}
	if len(calls) < 6 {
		t.Errorf("found %d call sites, expected the six subscription webhooks", len(calls))
	}
}

// A licence write that fails (the database, not a licence refused as
// unlinked, stale or restricted) fails the event, so Stripe sends it
// again, and nothing announces a change that was not made.
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
	for _, ev := range []struct {
		typ string
		obj map[string]any
	}{
		{"invoice.paid", inv},
		{"customer.subscription.updated", map[string]any{"id": subID, "object": "subscription", "status": "unpaid"}},
		{"customer.subscription.deleted", sub},
		{"invoice.payment_failed", inv},
		{"customer.subscription.paused", sub},
		{"customer.subscription.resumed", sub},
	} {
		id := "evt_failwrite_" + ev.typ + "_" + suffix
		if code, out := signedWebhook(t, h, secret, id, ev.typ, ev.obj); code != http.StatusInternalServerError || out["retry"] != true {
			t.Errorf("%s: a failed licence write was answered %d %v; want 500 so Stripe sends it again", ev.typ, code, out)
		}
		if s.IsEventProcessed(ctx, processedEventClaimProvider, id) || s.IsEventProcessed(ctx, processedEventDoneProvider, id) {
			t.Errorf("%s: event claim not released", ev.typ)
		}
	}
	time.Sleep(200 * time.Millisecond) // audit lines are written in the background
	var audits int
	if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ?", lic.ID).Scan(ctx, &audits); err != nil || audits != 0 {
		t.Fatalf("audit lines = %d (err %v), want none for a licence that was never written", audits, err)
	}
}
