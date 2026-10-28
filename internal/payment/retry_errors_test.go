package payment

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// The events that only send an email or write an audit line, and the
// once-per-episode notice of a failed payment, return a database that
// cannot be read: the event is retried rather than taken as "not ours"
// or the notice dropped.
func TestDatabaseErrorsAreRetried(t *testing.T) {
	s, ctx := openStore(t)
	h := &StripeHandler{Store: s}
	s.Close() // every query now fails

	for name, call := range map[string]func() error{
		"payment_action_required": func() error {
			return h.onPaymentActionRequired(ctx, []byte(`{"subscription":"sub_x","hosted_invoice_url":"https://x"}`))
		},
		"trial_will_end":   func() error { return h.onTrialWillEnd(ctx, []byte(`{"id":"sub_x","trial_end":1}`)) },
		"invoice.upcoming": func() error { return h.onInvoiceUpcoming(ctx, []byte(`{"subscription":"sub_x"}`)) },
		"payment_failed notice": func() error {
			at := time.Now()
			return h.notifyPaymentFailed(ctx, &model.License{ID: "lic_x", PastDueAt: &at}, "sub_x")
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: nil with the database down, want an error so Stripe sends it again", name)
		}
	}
}
