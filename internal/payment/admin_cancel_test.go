package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// The admin "cancel and revoke" puts an upgraded licence back on its
// free plan, as a refund in Stripe does, rather than leaving the caller
// to revoke a licence the customer had before paying.
func TestCancelForAdmin_RevertsAnUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "admcancel")
	sessionID, subID := "cs_adm_"+f.tag, "sub_adm_"+f.tag
	f.applySub(t, sessionID, subID)
	canceled := 0
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID {
			canceled++
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	reverted, err := f.h.CancelForAdmin(f.ctx, f.reload(t))
	if err != nil || !reverted {
		t.Fatalf("CancelForAdmin = %v, %v; want reverted", reverted, err)
	}
	if canceled != 1 {
		t.Fatalf("subscription cancelled %d times, want 1", canceled)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndAdmin)
}

// The customer's immediate cancel in the portal puts an upgraded licence
// back on its free plan too, and says nothing of an end: the licence goes
// on, free. The deletion Stripe sends next finds nothing to do.
func TestPortalImmediateCancel_RevertsAnUpgrade(t *testing.T) {
	f := newUpgradeFixture(t, "portalcancel")
	sessionID, subID := "cs_pc_"+f.tag, "sub_pc_"+f.tag
	f.applySub(t, sessionID, subID)
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/subscriptions/"+subID {
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/cancel",
		bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q,"immediate":true}`, f.lic.ID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", f.lic.Email)
	f.h.CancelSubscription(c)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	if err := f.h.onSubscriptionDeleted(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, subID))); err != nil {
		t.Fatal(err)
	}
	f.assertBackOnFree(t, sessionID, upgradeEndCanceled)
	time.Sleep(500 * time.Millisecond) // audit lines land in the background
	var n int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'canceled'", f.lic.ID).Scan(f.ctx, &n); err != nil || n != 0 {
		t.Fatalf("%d canceled lines (%v) for a licence that went back to free, want none", n, err)
	}
}

// Detached work ends before its claim could be taken over.
func TestWebhookWorkEndsBeforeItsClaimGoesStale(t *testing.T) {
	if webhookWorkTimeout >= staleClaimAge {
		t.Fatalf("webhookWorkTimeout %v must be shorter than staleClaimAge %v", webhookWorkTimeout, staleClaimAge)
	}
}

// The customer's immediate cancel of a plain subscription ends the
// licence and tells it once; the deletion Stripe sends next stays quiet.
func TestPortalImmediateCancel_ToldOnce(t *testing.T) {
	f := newUpgradeFixture(t, "portalplain")
	subID := "sub_pp_" + f.tag
	l := f.reload(t)
	since := time.Now().Add(-48 * time.Hour)
	l.StripeSubscriptionID, l.PaymentProvider, l.Status, l.PastDueAt = subID, "stripe", model.StatusPastDue, &since
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider", "status", "past_due_at"); err != nil {
		t.Fatal(err)
	}
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/portal/subscription/cancel",
		bytes.NewBufferString(fmt.Sprintf(`{"license_id":%q,"immediate":true}`, f.lic.ID)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", f.lic.Email)
	// The customer closes the page as soon as Stripe has the cancel: the
	// write and the notice still finish.
	closed, closePage := context.WithCancel(context.Background())
	closePage()
	c.Request = c.Request.WithContext(closed)
	f.h.CancelSubscription(c)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	if got := f.reload(t); got.Status != model.StatusCanceled || got.PastDueAt != nil {
		t.Fatalf("after the cancel: %s, past due since %v; want canceled, past due no more", got.Status, got.PastDueAt)
	}
	if err := f.h.onSubscriptionDeleted(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, subID))); err != nil {
		t.Fatal(err)
	}
	if st := f.reload(t).Status; st != model.StatusCanceled {
		t.Fatalf("status %s, want canceled", st)
	}
	time.Sleep(500 * time.Millisecond) // audit lines land in the background
	var n int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'canceled'", f.lic.ID).Scan(f.ctx, &n); err != nil || n != 1 {
		t.Fatalf("the end told %d times (%v), want once", n, err)
	}
}

// For a licence no upgrade moved, the subscription is cancelled and the
// caller revokes; a cancel Stripe cannot confirm is an error, so the
// caller changes nothing.
func TestCancelForAdmin_PlainSubscription(t *testing.T) {
	f := newUpgradeFixture(t, "admplain")
	subID := "sub_plain_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	fail := true
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && fail:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"boom"}}`)
		case r.Method == http.MethodDelete:
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"canceled"}`, subID)
		default: // the follow-up read: still active
			fmt.Fprintf(w, `{"id":"%s","object":"subscription","status":"active"}`, subID)
		}
	})
	if reverted, err := f.h.CancelForAdmin(f.ctx, f.reload(t)); err == nil || reverted {
		t.Fatalf("failed cancel = %v, %v; want an error", reverted, err)
	}
	fail = false
	if reverted, err := f.h.CancelForAdmin(f.ctx, f.reload(t)); err != nil || reverted {
		t.Fatalf("cancel = %v, %v; want cancelled, not reverted", reverted, err)
	}
}

// An unpaid subscription is never deleted, so the update that reports
// it is the only moment its licence ends: license.canceled goes out
// then, once, however often Stripe repeats the update.
func TestUnpaidSubscriptionSendsCanceledOnce(t *testing.T) {
	f := newUpgradeFixture(t, "unpaidev")
	var mu sync.Mutex
	var got []map[string]any
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Event == model.EventLicenseCanceled {
			mu.Lock()
			got = append(got, body.Data)
			mu.Unlock()
		}
	}))
	defer receiver.Close()
	if err := f.s.CreateWebhook(f.ctx, &model.Webhook{ProductID: f.lic.ProductID, URL: receiver.URL, Secret: "whsec_t",
		Active: true, Events: []string{model.EventLicenseCanceled}}); err != nil {
		t.Fatal(err)
	}
	f.h.WebhookSvc = service.NewWebhookService(f.s, slog.Default(), 2*time.Second, 1, true)

	subID := "sub_unpaid_" + f.tag
	l := f.reload(t)
	l.StripeSubscriptionID, l.PaymentProvider = subID, "stripe"
	if err := f.s.UpdateLicense(f.ctx, l, "stripe_subscription_id", "payment_provider"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.h.onSubscriptionUpdated(f.ctx, fmt.Appendf(nil, `{"id":"%s","status":"unpaid"}`, subID)); err != nil {
			t.Fatalf("unpaid update: %v", err)
		}
	}
	if s := f.reload(t).Status; s != model.StatusCanceled {
		t.Fatalf("status %q, want canceled", s)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // room for a second one to arrive
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0]["license_id"] != f.lic.ID || got[0]["reason"] != "subscription_unpaid" {
		t.Fatalf("license.canceled deliveries = %v, want one for %s with reason subscription_unpaid", got, f.lic.ID)
	}
}
