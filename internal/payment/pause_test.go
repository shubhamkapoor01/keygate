package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/tabloy/keygate/internal/model"
)

// pauseFixture is a subscription licence per test case, and a stubbed
// Stripe that answers each subscription with the status a case sets.
type pauseFixture struct {
	t      *testing.T
	h      *StripeHandler
	ctx    context.Context
	plan   *model.Plan
	suffix string
	mu     sync.Mutex
	says   map[string]string
	reads  int
	onRead func(id string) // runs as Stripe answers, when set
}

func newPauseFixture(t *testing.T) *pauseFixture {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	f := &pauseFixture{t: t, h: &StripeHandler{Store: s}, ctx: ctx, says: map[string]string{},
		plan:   seedPlan(t, s, ctx, "pause", "subscription"),
		suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")}
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()
	stubStripe(t, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/v1/subscriptions/"):]
		f.mu.Lock()
		status, hook := f.says[id], f.onRead
		f.reads++
		f.mu.Unlock()
		if hook != nil {
			hook(id)
		}
		fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":%q,"cancel_at_period_end":false,"items":{"object":"list","data":[{"id":"si_1","current_period_end":%d}]}}`,
			id, status, periodEnd)
	})
	return f
}

func (f *pauseFixture) licence(name, status, setup string) *model.License {
	f.t.Helper()
	until := time.Now().Add(5 * 24 * time.Hour)
	lic := &model.License{ProductID: f.plan.ProductID, PlanID: f.plan.ID, Email: name + "-" + f.suffix + "@example.com",
		LicenseKey: "KEY-PAUSE-" + name + "-" + f.suffix, Status: status, ValidUntil: &until,
		StripeSubscriptionID: "sub_pause_" + name + "_" + f.suffix}
	if err := f.h.Store.CreateLicense(f.ctx, lic); err != nil {
		f.t.Fatal(err)
	}
	if setup != "" {
		if _, err := f.h.Store.DB.NewRaw("UPDATE licenses SET "+setup+" WHERE id = ?", lic.ID).Exec(f.ctx); err != nil {
			f.t.Fatal(err)
		}
	}
	return lic
}

// stripeSays sets what Stripe answers for the licence's subscription.
func (f *pauseFixture) stripeSays(lic *model.License, status string) {
	f.mu.Lock()
	f.says[lic.StripeSubscriptionID] = status
	f.mu.Unlock()
}

// event delivers a subscription event the way the webhook routes it.
func (f *pauseFixture) event(lic *model.License) {
	f.t.Helper()
	if err := f.h.onSubscriptionUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID))); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pauseFixture) state(lic *model.License) string {
	f.t.Helper()
	var st string
	if err := f.h.Store.DB.NewRaw("SELECT status || ':' || COALESCE(suspended_by, '') FROM licenses WHERE id = ?", lic.ID).Scan(f.ctx, &st); err != nil {
		f.t.Fatal(err)
	}
	return st
}

// audits counts a licence's audit lines with the action, once the ones
// written in the background have had time to land.
func (f *pauseFixture) audits(lic *model.License, action string, want int) int {
	f.t.Helper()
	var n int
	for i := 0; i < 30; i++ {
		if err := f.h.Store.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = ?", lic.ID, action).Scan(f.ctx, &n); err != nil {
			f.t.Fatal(err)
		}
		if n >= want {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // and no more arrive
	_ = f.h.Store.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = ?", lic.ID, action).Scan(f.ctx, &n)
	return n
}

// A pause and a resume are read as Stripe has the subscription now: the
// pause suspends the licence and is told once, the resume lifts it, and
// a pause delivered after the resume changes nothing. An operator's
// suspension and an ended licence are left alone.
func TestStripePauseFollowsTheSubscription(t *testing.T) {
	f := newPauseFixture(t)

	lic := f.licence("paid", model.StatusActive, "")
	f.stripeSays(lic, "paused")
	f.event(lic) // customer.subscription.paused
	if st := f.state(lic); st != "suspended:stripe" {
		t.Fatalf("after the pause: %s, want suspended by stripe", st)
	}
	f.event(lic) // the same pause again, or its updated twin
	if n := f.audits(lic, "suspended", 1); n != 1 {
		t.Fatalf("pause told %d times, want once", n)
	}

	f.stripeSays(lic, "active")
	f.event(lic) // customer.subscription.resumed
	if st := f.state(lic); st != "active:" {
		t.Fatalf("after the resume: %s, want active", st)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("resume told %d times, want once", n)
	}

	f.event(lic) // the pause, delivered late: Stripe says active
	if st := f.state(lic); st != "active:" {
		t.Fatalf("a pause delivered after the resume: %s, want still active", st)
	}
	if n := f.audits(lic, "suspended", 1); n != 1 {
		t.Fatalf("late pause told again: %d", n)
	}

	operator := f.licence("operator", model.StatusSuspended, "suspended_by = 'admin', suspended_at = now()")
	f.stripeSays(operator, "paused")
	f.event(operator)
	f.stripeSays(operator, "active")
	f.event(operator)
	if st := f.state(operator); st != "suspended:admin" {
		t.Fatalf("an operator's suspension after a pause and a resume: %s", st)
	}

	ended := f.licence("ended", model.StatusCanceled, "canceled_at = now()")
	f.stripeSays(ended, "paused")
	f.event(ended)
	if st := f.state(ended); st != "canceled:" {
		t.Fatalf("a pause on an ended licence: %s, want canceled", st)
	}
}

// invoice.payment_failed follows the subscription as Stripe has it now:
// a failure Stripe no longer shows (the invoice was paid since) duns
// nobody, a past_due one is reported once, and a lookup that failed is
// retried rather than taken as "not ours".
func TestPaymentFailedFollowsTheSubscription(t *testing.T) {
	f := newPauseFixture(t)
	fail := func(lic *model.License) error {
		return f.h.onPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open"}`, lic.StripeSubscriptionID)))
	}

	reported := func(lic *model.License) int { // written in the request, not in the background
		t.Helper()
		var n int
		if err := f.h.Store.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'payment_failed:%'", lic.ID).Scan(f.ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	recovered := f.licence("recovered", model.StatusActive, "")
	f.stripeSays(recovered, "active")
	if err := fail(recovered); err != nil {
		t.Fatal(err)
	}
	if st := f.state(recovered); st != "active:" {
		t.Fatalf("a failure delivered after the invoice was paid: %s, want active", st)
	}
	if n := reported(recovered); n != 0 {
		t.Fatalf("a paid customer was reported as failing: %d", n)
	}

	dunning := f.licence("dunning", model.StatusActive, "")
	f.stripeSays(dunning, "past_due")
	for range 2 { // Stripe sends one for every failed retry
		if err := fail(dunning); err != nil {
			t.Fatal(err)
		}
	}
	if st := f.state(dunning); st != "past_due:" {
		t.Fatalf("a failure Stripe shows as past_due: %s", st)
	}
	if n := reported(dunning); n != 1 {
		t.Fatalf("past_due episode reported %d times, want once", n)
	}

	failing, cancel := context.WithCancel(f.ctx)
	cancel()
	if err := f.h.onPaymentFailed(failing, []byte(`{"subscription":"sub_any","status":"open"}`)); err == nil {
		t.Fatal("a failed lookup was answered as done; Stripe would not send it again")
	}
}

// Stripe sends resumed, updated and invoice.paid together, and all of
// them read the subscription as active. Whichever lands first lifts the
// pause; the end of the pause is told once, not once per event.
func TestStripeResumeToldOnceUnderRace(t *testing.T) {
	f := newPauseFixture(t)
	for round := range 5 {
		lic := f.licence(fmt.Sprintf("race%d", round), model.StatusSuspended, "suspended_by = 'stripe', suspended_at = now()")
		f.stripeSays(lic, "active")
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = f.h.onSubscriptionUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID)))
			}()
		}
		wg.Wait()
		if st := f.state(lic); st != "active:" {
			t.Fatalf("round %d: %s, want active", round, st)
		}
		if n := f.audits(lic, "reinstated", 1); n != 1 {
			t.Fatalf("round %d: resume told %d times, want once", round, n)
		}
	}
}

// Without an API key a paid invoice still lifts a Stripe pause from its
// payload, and says so.
func TestInvoicePaidPayloadToldWhenItLiftsAPause(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "pausepay", "subscription"),
		suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""), says: map[string]string{}}
	lic := f.licence("payload", model.StatusSuspended, "suspended_by = 'stripe', suspended_at = now()")
	if err := h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":1000,"period_end":%d}`,
		lic.StripeSubscriptionID, time.Now().Add(30*24*time.Hour).Unix()))); err != nil {
		t.Fatal(err)
	}
	if st := f.state(lic); st != "active:" {
		t.Fatalf("after the paid invoice: %s, want active", st)
	}
	if n := f.audits(lic, "reinstated", 1); n != 1 {
		t.Fatalf("lifting the pause told %d times, want once", n)
	}
}

// Stripe's last failed retry sends invoice.payment_failed and
// customer.subscription.updated together, both reading the subscription
// as unpaid. The licence ends once, and is said to end once.
func TestUnpaidToldOnceUnderRace(t *testing.T) {
	f := newPauseFixture(t)
	for round := range 5 {
		lic := f.licence(fmt.Sprintf("unpaid%d", round), model.StatusPastDue, "past_due_at = now()")
		f.stripeSays(lic, "unpaid")
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_ = f.h.onSubscriptionUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID)))
				} else {
					_ = f.h.onPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open"}`, lic.StripeSubscriptionID)))
				}
			}()
		}
		wg.Wait()
		if st := f.state(lic); st != "canceled:" {
			t.Fatalf("round %d: %s, want canceled", round, st)
		}
		if n := f.audits(lic, "canceled", 1); n != 1 {
			t.Fatalf("round %d: the end told %d times, want once", round, n)
		}
	}
}

// Without an API key, the $0 invoice that opened a trial, delivered after
// the pause, does not lift it.
func TestZeroInvoiceDoesNotLiftAPause(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "pausezero", "subscription"),
		suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""), says: map[string]string{}}
	lic := f.licence("zero", model.StatusSuspended, "suspended_by = 'stripe', suspended_at = now()")
	if err := h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":0,"period_end":%d}`,
		lic.StripeSubscriptionID, time.Now().Add(14*24*time.Hour).Unix()))); err != nil {
		t.Fatal(err)
	}
	if st := f.state(lic); st != "suspended:stripe" {
		t.Fatalf("a late $0 invoice: %s, want still suspended by stripe", st)
	}
	// Nor does it extend the paused licence: it paid for nothing.
	var until time.Time
	if err := s.DB.NewRaw("SELECT valid_until FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &until); err != nil {
		t.Fatal(err)
	}
	if !model.SameEnd(&until, lic.ValidUntil) { // Postgres keeps microseconds, Go may have nanoseconds
		t.Fatalf("valid_until %v after a $0 invoice on a paused licence, want %v", until, *lic.ValidUntil)
	}
}

// The webhook hands customer.subscription.paused and .resumed to the
// reading handler: what Stripe has now decides, not the event's payload.
func TestPauseEventsAreReadFromStripe(t *testing.T) {
	f := newPauseFixture(t)
	secret := "whsec_pause_" + f.suffix
	f.h.SetWebhookSecret(secret)
	lic := f.licence("routed", model.StatusActive, "")
	send := func(id, typ, payloadStatus string) {
		t.Helper()
		obj := map[string]any{"id": lic.StripeSubscriptionID, "object": "subscription", "status": payloadStatus}
		if code, out := signedWebhook(t, f.h, secret, id+"_"+f.suffix, typ, obj); code != http.StatusOK {
			t.Fatalf("%s: %d %v", typ, code, out)
		}
	}

	f.stripeSays(lic, "paused")
	send("evt_routed_p", "customer.subscription.paused", "active") // the payload disagrees
	if st := f.state(lic); st != "suspended:stripe" {
		t.Fatalf("paused event: %s, want suspended by stripe, as Stripe has it", st)
	}
	f.stripeSays(lic, "active")
	send("evt_routed_r", "customer.subscription.resumed", "paused")
	if st := f.state(lic); st != "active:" {
		t.Fatalf("resumed event: %s, want active, as Stripe has it", st)
	}
}

// Without an API key, a resume and the $0 invoice Stripe sends with it
// race. The invoice moves only the date, so whichever lands first the
// licence ends up active: a copy read before the resume cannot write the
// pause back.
func TestZeroInvoiceRacingAResume(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "pauserace", "subscription"),
		suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""), says: map[string]string{}}
	for round := range 8 {
		lic := f.licence(fmt.Sprintf("zr%d", round), model.StatusSuspended, "suspended_by = 'stripe', suspended_at = now()")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = h.onSubscriptionUpdated(ctx, []byte(fmt.Sprintf(`{"id":%q,"status":"active"}`, lic.StripeSubscriptionID)))
		}()
		go func() {
			defer wg.Done()
			_ = h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":0,"period_end":%d}`,
				lic.StripeSubscriptionID, time.Now().Add(30*24*time.Hour).Unix())))
		}()
		wg.Wait()
		if st := f.state(lic); st != "active:" {
			t.Fatalf("round %d: %s after a resume and a $0 invoice, want active", round, st)
		}
	}
}

// payment_failed and subscription.updated entering the same past_due
// episode at once keep one start: the failure is reported once, and
// Stripe's later retries of the invoice report nothing more.
func TestPastDueEpisodeKeepsItsStart(t *testing.T) {
	f := newPauseFixture(t)
	for round := range 5 {
		lic := f.licence(fmt.Sprintf("ep%d", round), model.StatusActive, "")
		f.stripeSays(lic, "past_due")
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_ = f.h.onSubscriptionUpdated(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID)))
				} else {
					_ = f.h.onPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open"}`, lic.StripeSubscriptionID)))
				}
			}()
		}
		wg.Wait()
		for range 2 { // Stripe's later retries of the invoice
			if err := f.h.onPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open"}`, lic.StripeSubscriptionID))); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := f.h.Store.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'payment_failed:%'", lic.ID).Scan(f.ctx, &n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("round %d: the episode reported %d times, want once", round, n)
		}
	}
}

// A licence's end is told once, by the write that ended it: an unpaid or
// canceled read, or the deletion, whichever finds it not yet canceled.
// The end clears a pause Stripe made, and keeps an operator's suspension
// on record.
func TestLicenceEndToldOnce(t *testing.T) {
	f := newPauseFixture(t)
	deleted := func(lic *model.License) {
		t.Helper()
		if err := f.h.onSubscriptionDeleted(f.ctx, []byte(fmt.Sprintf(`{"id":%q}`, lic.StripeSubscriptionID))); err != nil {
			t.Fatal(err)
		}
	}
	reason := func(lic *model.License) string {
		t.Helper()
		var r string
		_ = f.h.Store.DB.NewRaw("SELECT COALESCE(changes->>'reason', '') FROM audit_logs WHERE entity_id = ? AND action = 'canceled'", lic.ID).Scan(f.ctx, &r)
		return r
	}
	for _, c := range []struct {
		name, setup, stripe, wantState, wantReason string
	}{
		{"unpaid", "", "unpaid", "canceled:", "subscription_unpaid"},
		{"canceled", "", "canceled", "canceled:", "subscription_deleted"},
		{"deleted", "", "", "canceled:", "subscription_deleted"},
		{"paused", "status = 'suspended', suspended_by = 'stripe', suspended_at = now()", "", "canceled:", "subscription_deleted"},
		{"operator", "status = 'suspended', suspended_by = 'admin', suspended_at = now()", "", "canceled:admin", "subscription_deleted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status := model.StatusActive
			if c.setup != "" {
				status = model.StatusSuspended
			}
			lic := f.licence("end"+c.name, status, c.setup)
			if c.stripe != "" {
				f.stripeSays(lic, c.stripe)
				f.event(lic)
			}
			deleted(lic)
			deleted(lic) // Stripe sends it again
			if st := f.state(lic); st != c.wantState {
				t.Fatalf("state %s, want %s", st, c.wantState)
			}
			if n := f.audits(lic, "canceled", 1); n != 1 {
				t.Fatalf("the end told %d times, want once", n)
			}
			if r := reason(lic); r != c.wantReason {
				t.Fatalf("told as %q, want %q", r, c.wantReason)
			}
		})
	}
}

// Without an API key, several invoice.paid deliveries for a paused
// licence race: the end of the pause is told once, from the row as each
// write locked it.
func TestInvoicePaidPayloadToldOnceUnderRace(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s}
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "paidrace", "subscription"),
		suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""), says: map[string]string{}}
	for round := range 5 {
		lic := f.licence(fmt.Sprintf("ipr%d", round), model.StatusSuspended, "suspended_by = 'stripe', suspended_at = now()")
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":900,"period_end":%d}`,
					lic.StripeSubscriptionID, time.Now().Add(30*24*time.Hour).Unix())))
			}()
		}
		wg.Wait()
		if st := f.state(lic); st != "active:" {
			t.Fatalf("round %d: %s, want active", round, st)
		}
		if n := f.audits(lic, "reinstated", 1); n != 1 {
			t.Fatalf("round %d: the end of the pause told %d times, want once", round, n)
		}
	}
}

// payment_failed asks Stripe once, for the held change and the status
// alike; a licence deleted while Stripe answered leaves nothing to do.
func TestPaymentFailedReadsStripeOnce(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("once", model.StatusActive, "")
	f.stripeSays(lic, "past_due")
	f.mu.Lock()
	f.reads = 0
	f.mu.Unlock()
	if err := f.h.onPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open","billing_reason":"subscription_update"}`, lic.StripeSubscriptionID))); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	reads := f.reads
	f.mu.Unlock()
	if reads != 1 {
		t.Fatalf("Stripe read %d times, want once", reads)
	}
	if st := f.state(lic); st != "past_due:" {
		t.Fatalf("state %s, want past_due", st)
	}

	gone := f.licence("gone", model.StatusActive, "")
	f.stripeSays(gone, "past_due")
	f.mu.Lock()
	f.onRead = func(string) {
		_, _ = f.h.Store.DB.NewRaw("DELETE FROM licenses WHERE id = ?", gone.ID).Exec(context.Background())
	}
	f.mu.Unlock()
	t.Cleanup(func() { f.mu.Lock(); f.onRead = nil; f.mu.Unlock() })
	if err := f.h.onPaymentFailed(f.ctx, []byte(fmt.Sprintf(`{"subscription":%q,"status":"open"}`, gone.StripeSubscriptionID))); err != nil {
		t.Fatalf("licence deleted meanwhile: %v, want done", err)
	}
	var n int
	_ = f.h.Store.DB.NewRaw("SELECT count(*) FROM licenses WHERE id = ?", gone.ID).Scan(f.ctx, &n)
	if n != 0 {
		t.Fatal("the hook did not delete the licence")
	}
}

// A webhook's work does not stop when Stripe hangs up: a deletion whose
// request is gone still ends the licence and tells it, since a retry
// would find it ended and tell nothing.
func TestWebhookWorkOutlivesTheRequest(t *testing.T) {
	f := newPauseFixture(t)
	secret := "whsec_gone_" + f.suffix
	f.h.SetWebhookSecret(secret)
	lic := f.licence("hungup", model.StatusActive, "")
	body, _ := json.Marshal(map[string]any{"id": "evt_hungup_" + f.suffix, "object": "event", "type": "customer.subscription.deleted",
		"livemode": false, "api_version": stripe.APIVersion, "created": time.Now().Unix(),
		"data": map[string]any{"object": map[string]any{"id": lic.StripeSubscriptionID, "object": "subscription", "status": "canceled"}}})
	sig := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
	gone, cancel := context.WithCancel(context.Background())
	cancel() // Stripe has hung up
	req := httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(string(body))).WithContext(gone)
	req.Header.Set("Stripe-Signature", sig.Header)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/wh", f.h.Webhook)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if st := f.state(lic); st != "canceled:" {
		t.Fatalf("state %s after a deletion whose request was gone, want canceled", st)
	}
	// And the event is recorded as done: a resend skips it.
	var inFlight int
	if err := f.h.Store.DB.NewRaw("SELECT count(*) FROM processed_events WHERE provider = ? AND event_id = ?",
		processedEventClaimProvider, "evt_hungup_"+f.suffix).Scan(f.ctx, &inFlight); err != nil || inFlight != 0 {
		t.Fatalf("in-flight marker left (%d, %v): the event was not completed", inFlight, err)
	}
	if n := f.audits(lic, "canceled", 1); n != 1 {
		t.Fatalf("the end told %d times, want once", n)
	}
}

// A copy read in a past_due episode that has since ended does not lend
// the new episode its start: the row, read under its lock, decides.
func TestNewPastDueEpisodeStartsNow(t *testing.T) {
	f := newPauseFixture(t)
	lic := f.licence("episode", model.StatusActive, "")
	old := time.Now().Add(-30 * 24 * time.Hour)
	stale := *lic
	stale.Status, stale.PastDueAt = model.StatusPastDue, &old // read before the recovery
	asOf, err := f.h.Store.StripeReadStamp(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.applySubscriptionState(f.ctx, &stale, &subscriptionEvent{ID: lic.StripeSubscriptionID, Status: "past_due"}, &asOf, "customer.subscription.updated"); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	if err := f.h.Store.DB.NewRaw("SELECT past_due_at FROM licenses WHERE id = ?", lic.ID).Scan(f.ctx, &at); err != nil {
		t.Fatal(err)
	}
	if time.Since(at) > time.Minute {
		t.Fatalf("new episode starts %v, want now (not the old episode's %v)", at, old)
	}
}

// A recovery is told once however many events race to make it, and is
// told for the episode the row was in when the write locked it, even
// when the event's copy of the licence was read before it fell behind.
func TestRecoveryToldOnceFromTheRow(t *testing.T) {
	s, ctx := openStore(t)
	t.Cleanup(func() { s.Close() })
	h := &StripeHandler{Store: s} // no API key: invoice.paid and updated race on their payloads
	f := &pauseFixture{t: t, h: h, ctx: ctx, plan: seedPlan(t, s, ctx, "recrace", "subscription"),
		suffix: strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""), says: map[string]string{}}
	recovered := func(lic *model.License) []string {
		t.Helper()
		var tags []string
		if err := s.DB.NewRaw("SELECT tag FROM notifications WHERE license_id = ? AND tag LIKE 'payment_recovered%'", lic.ID).Scan(ctx, &tags); err != nil {
			t.Fatal(err)
		}
		return tags
	}
	for round := range 5 {
		lic := f.licence(fmt.Sprintf("rr%d", round), model.StatusPastDue, "past_due_at = now() - interval '3 days'")
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_ = h.onInvoicePaid(ctx, []byte(fmt.Sprintf(`{"subscription":%q,"amount_paid":900,"period_end":%d}`,
						lic.StripeSubscriptionID, time.Now().Add(30*24*time.Hour).Unix())))
				} else {
					_ = h.onSubscriptionUpdated(ctx, []byte(fmt.Sprintf(`{"id":%q,"status":"active"}`, lic.StripeSubscriptionID)))
				}
			}()
		}
		wg.Wait()
		if tags := recovered(lic); len(tags) != 1 {
			t.Fatalf("round %d: recovery told %v, want once", round, tags)
		}
		var left *time.Time
		if err := s.DB.NewRaw("SELECT past_due_at FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &left); err != nil || left != nil {
			t.Fatalf("round %d: past_due_at %v (%v) after the recovery, want cleared", round, left, err)
		}
	}

	// The copy says active; the row fell behind meanwhile.
	behind := f.licence("behind", model.StatusActive, "")
	var since time.Time
	if err := s.DB.NewRaw("UPDATE licenses SET status = 'past_due', past_due_at = now() - interval '2 days' WHERE id = ? RETURNING past_due_at", behind.ID).Scan(ctx, &since); err != nil {
		t.Fatal(err)
	}
	asOf, err := s.StripeReadStamp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.applySubscriptionState(ctx, behind, &subscriptionEvent{ID: behind.StripeSubscriptionID, Status: "active"}, &asOf, "customer.subscription.updated"); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("payment_recovered:%d", since.Unix())
	if tags := recovered(behind); len(tags) != 1 || tags[0] != want {
		t.Fatalf("recovery of a row the copy missed: %v, want [%s]", tags, want)
	}
}

// A past-due licence whose subscription ends has not recovered: no
// "payment recovered" goes out, whether it ends unpaid or canceled.
func TestPastDueThatEndsIsNoRecovery(t *testing.T) {
	f := newPauseFixture(t)
	for _, end := range []string{"unpaid", "canceled"} {
		lic := f.licence("pdend"+end, model.StatusPastDue, "past_due_at = now() - interval '5 days'")
		f.stripeSays(lic, end)
		f.event(lic)
		if st := f.state(lic); st != "canceled:" {
			t.Fatalf("%s: state %s, want canceled", end, st)
		}
		var n int
		if err := f.h.Store.DB.NewRaw("SELECT count(*) FROM notifications WHERE license_id = ? AND tag LIKE 'payment_recovered%'", lic.ID).Scan(f.ctx, &n); err != nil || n != 0 {
			t.Fatalf("%s: %d recovery notices (%v), want none", end, n, err)
		}
	}
}
