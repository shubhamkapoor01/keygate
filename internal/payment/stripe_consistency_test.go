package payment

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"

	"github.com/tabloy/keygate/internal/model"
)

// A delayed payment that fails at a subscription checkout leaves the
// subscription Stripe made (it voids only the first invoice). With no
// licence on it, it is cancelled when Keygate sold it (the session's
// metadata, or a price a plan sells); one a licence does follow, or one
// another product sold, is left.
func TestAsyncPaymentFailureCancelsTheOrphanSubscription(t *testing.T) {
	f := newPauseFixture(t)
	price := "price_async_" + f.suffix
	if _, err := f.h.Store.DB.NewRaw("UPDATE plans SET stripe_price_id = ? WHERE id = ?", price, f.plan.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	var cancels atomic.Int32
	orphan, priced, foreign, kept := "sub_orphan_"+f.suffix, "sub_priced_"+f.suffix, "sub_foreign_"+f.suffix, "sub_kept_"+f.suffix
	foreignTagged := "sub_foreign2_" + f.suffix
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && (r.URL.Path == "/v1/subscriptions/"+orphan || r.URL.Path == "/v1/subscriptions/"+priced):
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"canceled"}`, strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+priced:
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","items":{"object":"list","data":[{"id":"si_p","object":"subscription_item","quantity":1,"price":{"id":%q,"object":"price"}}]}}`, priced, price)
		case r.Method == http.MethodGet && (r.URL.Path == "/v1/subscriptions/"+foreign || r.URL.Path == "/v1/subscriptions/"+foreignTagged):
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","items":{"object":"list","data":[{"id":"si_f","object":"subscription_item","quantity":1,"price":{"id":"price_other_app","object":"price"}}]}}`, strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/"))
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	fail := func(session, subscription string) {
		t.Helper()
		if err := f.h.onAsyncPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"id":"cs_%s_%s","object":"checkout.session",%s}`, session, f.suffix, subscription))); err != nil {
			t.Fatal(err)
		}
	}
	fail("orphan", fmt.Sprintf(`"subscription":%q,"metadata":{"plan_id":%q}`, orphan, f.plan.ID))
	if n := cancels.Load(); n != 1 {
		t.Fatalf("orphan subscription cancelled %d times, want once", n)
	}
	// The subscription expanded into the session reads the same, and a
	// session without metadata is Keygate's by its price.
	fail("priced", fmt.Sprintf(`"subscription":{"id":%q,"object":"subscription","status":"active"}`, priced))
	if n := cancels.Load(); n != 2 {
		t.Fatalf("subscription of a plan's price: cancelled %d times in all, want 2", n)
	}
	fail("foreign", fmt.Sprintf(`"subscription":%q`, foreign))
	// Another product's metadata may use the same keys.
	fail("foreign2", fmt.Sprintf(`"subscription":%q,"metadata":{"plan_id":"pro","kind":"upgrade"}`, foreignTagged))
	if n := cancels.Load(); n != 2 {
		t.Fatal("a subscription another product sold was cancelled")
	}
	lic := f.licence("kept", model.StatusActive, "")
	if _, err := f.h.Store.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", kept, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	fail("kept", fmt.Sprintf(`"subscription":%q,"metadata":{"plan_id":%q}`, kept, f.plan.ID))
	if n := cancels.Load(); n != 2 {
		t.Fatal("a subscription a licence follows was cancelled")
	}
}

// Without an API key nothing can be cancelled or looked up: the event is
// acknowledged rather than retried for days.
func TestAsyncPaymentFailureWithoutAKey(t *testing.T) {
	f := newPauseFixture(t)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Stripe called without a key: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusUnauthorized)
	})
	stripe.Key = "" // restored by stubStripe's cleanup
	if err := f.h.onAsyncPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"id":"cs_nokey_%s","object":"checkout.session","subscription":"sub_nokey_%s"}`, f.suffix, f.suffix))); err != nil {
		t.Fatalf("without a key: %v", err)
	}
}

// An unpaid subscription ends its licence; paid later, Stripe revives
// it, and the licence comes back: no longer canceled, and told once.
func TestRevivedFromUnpaidIsToldOnce(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("revive", model.StatusPastDue, "past_due_at = now() - interval '20 days'")
	f.stripeSays(lic, "unpaid")
	f.event(lic)
	if st := f.state(lic); st != "canceled:" {
		t.Fatalf("unpaid: %s, want canceled", st)
	}
	f.stripeSays(lic, "active")
	f.event(lic)
	f.event(lic) // the same news again
	var status string
	var canceledAt *time.Time
	if err := f.h.Store.DB.NewRaw("SELECT status, canceled_at FROM licenses WHERE id = ?", lic.ID).Scan(f.ctx, &status, &canceledAt); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusActive || canceledAt != nil {
		t.Fatalf("revived: %s, canceled at %v; want active and no longer canceled", status, canceledAt)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("revival told %d times, want once", n)
	}
}

// Past due too long, the sweep expires the licence; paid later, Stripe
// makes the subscription active and the licence comes back, told once.
func TestRevivedAfterExpiryIsToldOnce(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("expired", model.StatusExpired, "")
	f.stripeSays(lic, "active")
	f.event(lic)
	f.event(lic) // the same news again
	if st := f.state(lic); st != "active:" {
		t.Fatalf("paid after expiry: %s, want active", st)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("revival told %d times, want once", n)
	}
}

// customer.updated writes the address Stripe has now, not the one in a
// payload that may be older than another change.
func TestCustomerEmailFollowsStripe(t *testing.T) {
	f := newPauseFixture(t)
	customer := "cus_mail_" + f.suffix
	lic := f.licence("mail", model.StatusActive, "stripe_customer_id = '"+customer+"'")
	current := "now-" + f.suffix + "@example.com"
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/customers/"+customer {
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"object":"customer","email":%q}`, customer, current)
	})
	if err := f.h.onCustomerUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q,"email":"older@example.com"}`, customer))); err != nil {
		t.Fatal(err)
	}
	var email string
	if err := f.h.Store.DB.NewRaw("SELECT email FROM licenses WHERE id = ?", lic.ID).Scan(f.ctx, &email); err != nil || email != current {
		t.Fatalf("email %q (%v), want Stripe's current %q", email, err, current)
	}
}

// Without an API key, an invoice paid from the customer's credit
// balance (amount_paid 0, a total) is a payment: it ends a trial.
func TestCreditPaidInvoiceEndsATrial(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "credit", "subscription"),
		suffix: fmt.Sprint(time.Now().UnixNano()), says: map[string]string{}}
	lic := f.licence("credit", model.StatusTrialing, "")
	if err := h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":0,"total":900,"period_end":%d}`,
		lic.StripeSubscriptionID, time.Now().Add(30*24*time.Hour).Unix()))); err != nil {
		t.Fatal(err)
	}
	if st := f.state(lic); st != "active:" {
		t.Fatalf("after a credit-paid invoice: %s, want active", st)
	}
}

// A full refund whose subscription cannot be cancelled changes nothing,
// and is retried: revoking alone would leave Stripe billing it.
func TestRefundKeepsTheLicenceWhenTheCancelFails(t *testing.T) {
	f := newUpgradeFixture(t, "refundfail")
	subID := "sub_rf_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/invoice_payments":
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_1","object":"invoice_payment","status":"paid",
				"invoice":{"id":"in_rf","object":"invoice","billing_reason":"subscription_cycle","parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}}]}`, subID)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"boom"}}`))
		default: // the read after a failed cancel: still billing
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active"}`, subID)
		}
	})
	err := f.h.onChargeRefunded(f.ctx, []byte(fmt.Sprintf(`{"id":"ch_rf_%s","payment_intent":"pi_rf_%s","refunded":true}`, f.tag, f.tag)))
	if err == nil {
		t.Fatal("refund with a failed cancel: nil, want an error so Stripe sends it again")
	}
	if st := f.reload(t).Status; st == model.StatusRevoked {
		t.Fatal("revoked while the subscription may still bill")
	}
}

// Cancelling at period end with an answer that carries no cancel_at
// leaves valid_until alone rather than setting it to 1970.
func TestCancelAtPeriodEndWithoutADate(t *testing.T) {
	f := newUpgradeFixture(t, "noperiod")
	subID := "sub_np_" + f.tag
	l := f.reload(t)
	until := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	l.StripeSubscriptionID, l.PaymentProvider, l.ValidUntil = subID, "stripe", &until
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "valid_until"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":true}`, subID)
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/cancel",
		bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q}`, f.lic.ID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", f.lic.Email)
	f.h.CancelSubscription(c)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	if got := f.reload(t).ValidUntil; got == nil || !model.SameEnd(got, &until) {
		t.Fatalf("valid_until %v, want %v kept", got, until)
	}
}

// A refunded invoice that bills a subscription no licence follows (an
// unlinked one, or another product's) is not one of ours: the
// customer's only licence is neither revoked nor its own subscription
// cancelled.
func TestRefundOfAnotherSubscriptionLeavesTheLicence(t *testing.T) {
	f := newUpgradeFixture(t, "othersub")
	own, other := "sub_own_"+f.tag, "sub_other_"+f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider, l.StripeCustomerID = own, "stripe", "cus_os_"+f.tag
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "stripe_customer_id"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/invoice_payments" {
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_1","object":"invoice_payment","status":"paid",
				"invoice":{"id":"in_os","object":"invoice","billing_reason":"subscription_cycle","parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}}]}`, other)
			return
		}
		t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
	})
	if err := f.h.onChargeRefunded(f.ctx, []byte(fmt.Sprintf(`{"id":"ch_os_%s","customer":"cus_os_%s","payment_intent":"pi_os_%s","refunded":true}`, f.tag, f.tag, f.tag))); err != nil {
		t.Fatal(err)
	}
	if st := f.reload(t).Status; st != model.StatusActive {
		t.Fatalf("status %s after refunding another subscription's invoice, want active", st)
	}
}

// A restricted key that may not read customers falls back to the
// event's email instead of retrying it for days.
func TestCustomerEmailWithAKeyThatCannotReadCustomers(t *testing.T) {
	f := newPauseFixture(t)
	customer := "cus_rk_" + f.suffix
	lic := f.licence("rk", model.StatusActive, "stripe_customer_id = '"+customer+"'")
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"The provided key does not have the required permissions"}}`))
	})
	payload := "from-event-" + f.suffix + "@example.com"
	if err := f.h.onCustomerUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q,"email":%q}`, customer, payload))); err != nil {
		t.Fatalf("forbidden read: %v, want the event applied", err)
	}
	var email string
	if err := f.h.Store.DB.NewRaw("SELECT email FROM licenses WHERE id = ?", lic.ID).Scan(f.ctx, &email); err != nil || email != payload {
		t.Fatalf("email %q (%v), want the event's %q", email, err, payload)
	}
}

// A refunded charge that only the customer ties to the licence (a
// one-off charge with no invoice) may have paid for something else:
// the subscription, which a cancel would end for good, is left.
func TestRefundFoundThroughTheCustomerLeavesTheSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "oneoff")
	subID, cus := "sub_oo_"+f.tag, "cus_oo_"+f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider, l.StripeCustomerID = subID, "stripe", cus
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "stripe_customer_id"); err != nil {
		t.Fatal(err)
	}
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/invoice_payments":
			fmt.Fprint(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[]}`)
		case r.Method == http.MethodDelete:
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"canceled"}`, subID)
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	if err := f.h.onChargeRefunded(f.ctx, []byte(fmt.Sprintf(`{"id":"ch_oo_%s","customer":%q,"payment_intent":"pi_oo_%s","refunded":true}`, f.tag, cus, f.tag))); err != nil {
		t.Fatal(err)
	}
	if n := cancels.Load(); n != 0 {
		t.Fatalf("subscription cancelled %d times for a charge found through the customer, want 0", n)
	}
}

// A licence already revoked (by an admin, or by a refund before
// refunds cancelled) can still be billed; refunding its invoice ends
// the subscription.
func TestRefundOfARevokedLicenceCancelsTheSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "revoked")
	subID := "sub_rv_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider, l.Status = subID, "stripe", model.StatusRevoked
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "status"); err != nil {
		t.Fatal(err)
	}
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/invoice_payments":
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_1","object":"invoice_payment","status":"paid",
				"invoice":{"id":"in_rv","object":"invoice","billing_reason":"subscription_cycle","parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}}]}`, subID)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID:
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"canceled"}`, subID)
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	if err := f.h.onChargeRefunded(f.ctx, []byte(fmt.Sprintf(`{"id":"ch_rv_%s","payment_intent":"pi_rv_%s","refunded":true}`, f.tag, f.tag))); err != nil {
		t.Fatal(err)
	}
	if n := cancels.Load(); n != 1 {
		t.Fatalf("subscription of a revoked licence cancelled %d times after a full refund, want 1", n)
	}
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s, want revoked", st)
	}
}

// A licence found by a one-time payment (data that also names a
// subscription) is revoked when that payment is refunded; the
// subscription, which that money did not pay for, is left.
func TestRefundOfAOneTimePaymentLeavesTheSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "ownpi")
	subID, pi := "sub_pi_"+f.tag, "pi_own_"+f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider, l.StripePaymentIntentID = subID, "stripe", pi
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "stripe_payment_intent_id"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
	})
	if err := f.h.onChargeRefunded(f.ctx, []byte(fmt.Sprintf(`{"id":"ch_pi_%s","payment_intent":%q,"refunded":true}`, f.tag, pi))); err != nil {
		t.Fatal(err)
	}
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s, want revoked", st)
	}
}

// A lost dispute takes the payment back for good and is handled as a
// full refund of it, once; a won one leaves the licence.
func TestLostDisputeIsAFullRefund(t *testing.T) {
	f := newUpgradeFixture(t, "dispute")
	pi := "pi_dp_" + f.tag
	l := f.reload(t)
	l.PaymentProvider, l.StripePaymentIntentID = "stripe", pi
	if err := f.s.UpdateLicense(f.ctx, l, "payment_provider", "stripe_payment_intent_id"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/charges/ch_dp_"+f.tag {
			fmt.Fprintf(w, `{"id":"ch_dp_%s","object":"charge","payment_intent":%q,"metadata":{}}`, f.tag, pi)
			return
		}
		t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
	})
	closed := func(status string) {
		t.Helper()
		if err := f.h.onDisputeClosed(f.ctx, []byte(fmt.Sprintf(`{"id":"dp_%s","object":"dispute","charge":"ch_dp_%s","payment_intent":%q,"amount":900,"status":%q}`, f.tag, f.tag, pi, status))); err != nil {
			t.Fatal(err)
		}
	}
	closed("won")
	if st := f.reload(t).Status; st == model.StatusRevoked {
		t.Fatal("a won dispute revoked the licence")
	}
	closed("lost")
	closed("lost") // delivered again
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s after a lost dispute, want revoked", st)
	}
	if n := auditCountAtLeast(f.s, f.ctx, l.ID, "revoked", 1); n != 1 {
		t.Fatalf("revoked %d times, want once", n)
	}
}

// A lost dispute ends the subscription with the licence, even over a
// period already over (disputes close months after the charge): billing
// the same card again would invite another.
func TestLostDisputeCancelsTheSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "dpsub")
	subID, pi := "sub_dps_"+f.tag, "pi_dps_"+f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	var cancels atomic.Int32
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/invoice_payments":
			fmt.Fprintf(w, `{"object":"list","has_more":false,"url":"/v1/invoice_payments","data":[{"id":"inpay_1","object":"invoice_payment","status":"paid",
				"invoice":{"id":"in_dps","object":"invoice","billing_reason":"subscription_cycle","parent":{"type":"subscription_details","subscription_details":{"subscription":%q}},
				"lines":{"object":"list","has_more":false,"data":[{"id":"il_dps","object":"line_item","period":{"start":%d,"end":%d}}]}}}]}`,
				subID, time.Now().Add(-90*24*time.Hour).Unix(), time.Now().Add(-60*24*time.Hour).Unix())
		case r.Method == http.MethodGet && r.URL.Path == "/v1/charges/ch_dps_"+f.tag:
			fmt.Fprintf(w, `{"id":"ch_dps_%s","object":"charge","payment_intent":%q,"metadata":{}}`, f.tag, pi)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID:
			cancels.Add(1)
			fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"canceled"}`, subID)
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
		}
	})
	if err := f.h.onDisputeClosed(f.ctx, []byte(fmt.Sprintf(`{"id":"dp_%s","object":"dispute","charge":"ch_dps_%s","payment_intent":%q,"amount":900,"status":"lost"}`, f.tag, f.tag, pi))); err != nil {
		t.Fatal(err)
	}
	if n := cancels.Load(); n != 1 {
		t.Fatalf("subscription cancelled %d times after a lost dispute, want 1", n)
	}
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s, want revoked", st)
	}
}

// A dispute's charge reads the same as an ID or expanded.
func TestDisputeCreatedWithAnExpandedCharge(t *testing.T) {
	f := newPauseFixture(t)
	id := "dp_exp_" + f.suffix
	f.h.onDisputeCreated(f.ctx, []byte(fmt.Sprintf(`{"id":%q,"object":"dispute","charge":{"id":"ch_exp_%s","object":"charge"},"status":"needs_response","amount":900}`, id, f.suffix)))
	var charge string
	for range 40 {
		_ = f.h.Store.DB.NewRaw("SELECT changes->>'charge_id' FROM audit_logs WHERE entity_id = ? AND action = 'created'", id).Scan(f.ctx, &charge)
		if charge != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if charge != "ch_exp_"+f.suffix {
		t.Fatalf("dispute noted with charge %q, want ch_exp_%s", charge, f.suffix)
	}
}

// Past due too long, then still past due in Stripe: not paid, so the
// licence stays expired (a new dunning episode would give it another
// month for nothing) and is not told it is back.
func TestPastDueAfterExpiryIsNotAReinstatement(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("pdexp", model.StatusExpired, "past_due_at = now() - interval '31 days'")
	f.stripeSays(lic, "past_due")
	f.event(lic)
	if st := f.state(lic); st != "expired:" {
		t.Fatalf("still past due: %s, want expired", st)
	}
	if n := f.audits(lic, "reinstated", 0); n != 0 {
		t.Fatalf("still past due, told reinstated %d times", n)
	}
}

// A licence that expired at the end of its grace (not past due first)
// still goes into dunning when its renewal fails.
func TestGraceExpiredThenPastDueGoesIntoDunning(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("graceexp", model.StatusExpired, "")
	f.stripeSays(lic, "past_due")
	f.event(lic)
	if st := f.state(lic); !strings.HasPrefix(st, "past_due") {
		t.Fatalf("renewal failed after the grace expiry: %s, want past_due", st)
	}
}

// A bank debit's dispute (SEPA, ACH) is created lost and final: taken
// back on creation, once, however many of its events say so. One still
// open changes nothing.
func TestDisputeCreatedLostIsTakenBack(t *testing.T) {
	f := newUpgradeFixture(t, "dpbank")
	pi := "pi_bank_" + f.tag
	l := f.reload(t)
	l.PaymentProvider, l.StripePaymentIntentID = "stripe", pi
	if err := f.s.UpdateLicense(f.ctx, l, "payment_provider", "stripe_payment_intent_id"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/charges/ch_bank_"+f.tag {
			fmt.Fprintf(w, `{"id":"ch_bank_%s","object":"charge","payment_intent":%q,"metadata":{}}`, f.tag, pi)
			return
		}
		t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotFound)
	})
	dispute := func(status string) []byte {
		return []byte(fmt.Sprintf(`{"id":"dp_bank_%s","object":"dispute","charge":"ch_bank_%s","payment_intent":%q,"amount":900,"status":%q}`, f.tag, f.tag, pi, status))
	}
	if err := f.h.onDisputeCreated(f.ctx, dispute("needs_response")); err != nil {
		t.Fatal(err)
	}
	if st := f.reload(t).Status; st == model.StatusRevoked {
		t.Fatal("an open dispute revoked the licence")
	}
	if err := f.h.onDisputeCreated(f.ctx, dispute("lost")); err != nil {
		t.Fatal(err)
	}
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s after a dispute created lost, want revoked", st)
	}
	if err := f.h.onDisputeClosed(f.ctx, dispute("lost")); err != nil {
		t.Fatal(err)
	}
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s after a dispute created lost, want revoked", st)
	}
	if n := auditCountAtLeast(f.s, f.ctx, l.ID, "revoked", 1); n != 1 {
		t.Fatalf("revoked told %d times, want once", n)
	}
}

// Takebacks of the same purchase racing (a refund delivered twice under
// different event ids, a refund and a lost dispute) revoke it once and
// tell it once.
func TestConcurrentTakeBacksRevokeOnce(t *testing.T) {
	f := newUpgradeFixture(t, "racetb")
	pi := "pi_race_" + f.tag
	l := f.reload(t)
	l.PaymentProvider, l.StripePaymentIntentID = "stripe", pi
	if err := f.s.UpdateLicense(f.ctx, l, "payment_provider", "stripe_payment_intent_id"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cause := "refund"
			if i%2 == 1 {
				cause = "dispute_lost"
			}
			if err := f.h.takeBackCharge(f.ctx, refundedCharge{ID: "ch_race_" + f.tag, PaymentIntent: pi, Refunded: true}, cause, time.Now().Unix()); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s, want revoked", st)
	}
	time.Sleep(300 * time.Millisecond) // let every audit line land
	if n := auditCountAtLeast(f.s, f.ctx, l.ID, "revoked", 1); n != 1 {
		t.Fatalf("revoked told %d times, want once", n)
	}
}

// A revocation written over one already made says so, so only the
// first is told.
func TestRevokingARevokedLicenceSaysSo(t *testing.T) {
	f := newUpgradeFixture(t, "wasrev")
	first, second := f.reload(t), f.reload(t) // two events' copies
	first.Status = model.StatusRevoked
	if err := f.s.UpdateLicenseAndSubscription(f.ctx, first, "status"); err != nil || first.WasRevoked {
		t.Fatalf("first revocation: %v, was revoked %v", err, first.WasRevoked)
	}
	second.Status = model.StatusRevoked
	if err := f.s.UpdateLicenseAndSubscription(f.ctx, second, "status"); err != nil || !second.WasRevoked {
		t.Fatalf("second revocation: %v, was revoked %v; want it to see the first", err, second.WasRevoked)
	}
}

// A key that may not read charges still takes back what a lost dispute
// paid for, found through the dispute's payment intent.
func TestLostDisputeWithAKeyThatCannotReadCharges(t *testing.T) {
	f := newUpgradeFixture(t, "dp403")
	pi := "pi_dp403_" + f.tag
	l := f.reload(t)
	l.PaymentProvider, l.StripePaymentIntentID = "stripe", pi
	if err := f.s.UpdateLicense(f.ctx, l, "payment_provider", "stripe_payment_intent_id"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"The provided key does not have the required permissions"}}`))
	})
	if err := f.h.onDisputeClosed(f.ctx, []byte(fmt.Sprintf(`{"id":"dp403_%s","object":"dispute","charge":"ch_dp403_%s","payment_intent":%q,"amount":900,"status":"lost"}`, f.tag, f.tag, pi))); err != nil {
		t.Fatalf("lost dispute with a restricted key: %v", err)
	}
	if st := f.reload(t).Status; st != model.StatusRevoked {
		t.Fatalf("status %s, want revoked", st)
	}
}

// An invoice is past when its lines have all ended; one still running,
// a list cut short (the period's own line may be on a later page), or
// none to go by, is current.
func TestPastInvoice(t *testing.T) {
	now := time.Now().Unix()
	inv := func(more bool, ends ...int64) *stripe.Invoice {
		lines := &stripe.InvoiceLineItemList{}
		lines.HasMore = more
		for _, e := range ends {
			lines.Data = append(lines.Data, &stripe.InvoiceLineItem{Period: &stripe.Period{End: e}})
		}
		return &stripe.Invoice{ID: "in_x", Lines: lines, Parent: &stripe.InvoiceParent{SubscriptionDetails: &stripe.InvoiceParentSubscriptionDetails{Subscription: &stripe.Subscription{ID: "sub_x"}}}}
	}
	for _, c := range []struct {
		name string
		inv  *stripe.Invoice
		at   int64
		want bool
	}{
		{"ended", inv(false, now-10), now, true},
		{"running", inv(false, now+10), now, false},
		{"one line running", inv(false, now-10, now+10), now, false},
		{"ended, list cut short", inv(true, now-10, now-20), now, false},
		{"no lines", inv(false), now, false},
		{"a lost dispute", inv(false, now-10), 0, false},
		{"not a subscription's", &stripe.Invoice{ID: "in_y", Lines: inv(false, now-10).Lines}, now, false},
	} {
		if got := pastInvoice(c.inv, c.at); got != c.want {
			t.Errorf("%s: past %v, want %v", c.name, got, c.want)
		}
	}
}

// customer.updated for a customer holding no licence reads nothing.
func TestCustomerUpdatedForAnotherProductsCustomer(t *testing.T) {
	f := newPauseFixture(t)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusTooManyRequests)
	})
	if err := f.h.onCustomerUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":"cus_other_%s","email":"x@example.com"}`, f.suffix))); err != nil {
		t.Fatal(err)
	}
}

// A payload (Stripe could not be read) that brings back a licence
// expired for non-payment says so once, as the write it makes.
func TestPayloadRevivalOfAnExpiredLicenceIsTold(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("plexp", model.StatusExpired, "past_due_at = now() - interval '31 days'")
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such subscription"}}`))
	})
	end := time.Now().Add(20 * 24 * time.Hour).Unix()
	for range 2 {
		if err := f.h.onSubscriptionUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q,"status":"active","items":{"data":[{"current_period_end":%d}]}}`, lic.StripeSubscriptionID, end))); err != nil {
			t.Fatal(err)
		}
	}
	if st := f.state(lic); st != "active:" {
		t.Fatalf("payload: %s, want active", st)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("told reinstated %d times, want once", n)
	}
}

// Unpaid ends the licence; paid and past due again before any delivery
// read it active, it is still back, and told so once.
func TestCanceledThenPastDueIsToldBack(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("cpd", model.StatusCanceled, "canceled_at = now()")
	f.stripeSays(lic, "past_due")
	f.event(lic)
	f.event(lic)
	if st := f.state(lic); st != "past_due:" {
		t.Fatalf("paid and past due again: %s, want past_due", st)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("told reinstated %d times, want once", n)
	}
}

// An operator's suspension that non-payment then ended (canceled, the
// suspension still on record) is not lifted by a later payment: payment
// gives back what non-payment took, never what an operator took.
func TestAdminSuspensionOutlivesUnpaidThenPaid(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("adminup", model.StatusCanceled, "canceled_at = now(), suspended_by = '"+model.SuspendedByAdmin+"', suspended_at = now()")
	f.stripeSays(lic, "active")
	f.event(lic)
	if st := f.state(lic); st != "canceled:"+model.SuspendedByAdmin {
		t.Fatalf("paid after an operator's suspension: %s, want still canceled and suspended by the operator", st)
	}
	if n := f.audits(lic, "reinstated", 0); n != 0 {
		t.Fatalf("told reinstated %d times", n)
	}
}

// Without a current read, a paid invoice (money came in) brings back a
// licence expired for non-payment, and says so once.
func TestPaidInvoicePayloadTellsAnExpiredLicenceIsBack(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "paidexp", "subscription"),
		suffix: fmt.Sprint(time.Now().UnixNano()), says: map[string]string{}}
	lic := f.licence("paidexp", model.StatusExpired, "past_due_at = now() - interval '31 days'")
	paid := []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":900,"total":900,"period_end":%d}`,
		lic.StripeSubscriptionID, time.Now().Add(30*24*time.Hour).Unix()))
	for range 2 {
		if err := h.onInvoicePaid(ctx, paid); err != nil {
			t.Fatal(err)
		}
	}
	if st := f.state(lic); st != "active:" {
		t.Fatalf("paid: %s, want active", st)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("told reinstated %d times, want once", n)
	}
}
