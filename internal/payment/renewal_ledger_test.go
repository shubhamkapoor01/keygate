package payment

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Random histories of renewals, refunds (in any order, some repeated),
// admin edits and refunds recorded before the renewal applied, run
// through the real store, end where a customer who never bought the
// refunded renewals would, with every admin edit kept as the shift it
// made when it was made. Lapsed periods are covered by start dates up to
// 400 days either side of now. The seed is fixed, so a failure repeats.
func TestRenewalLedgerMatchesTheModel(t *testing.T) {
	s, ctx := openStore(t)
	defer s.Close()
	plan := seedMaintenancePlan(t, s, ctx, "ledgerprop")
	rng := rand.New(rand.NewSource(1))
	failures := 0
	for h := 0; h < 120 && failures < 5; h++ {
		if msg := ledgerHistory(t, s, ctx, plan, rng, h); msg != "" {
			failures++
			t.Errorf("history %d:\n%s", h, msg)
		}
	}
}

type ledgerEvent struct {
	kind    string // renew, refund, edit, early
	days    int
	created time.Time
	id      string
	delta   time.Duration
}

func ledgerHistory(t *testing.T, s *store.Store, ctx context.Context, plan *model.Plan, rng *rand.Rand, h int) string {
	t.Helper()
	day := 24 * time.Hour
	start := time.Now().Add(time.Duration(rng.Intn(801)-400) * day).Truncate(time.Second)
	tag := fmt.Sprintf("%s-%d-%d", plan.Slug, h, rng.Int63())
	lic := &model.License{
		ProductID: plan.ProductID, PlanID: plan.ID, Email: "p" + tag + "@example.com",
		LicenseKey: "KEY-" + tag, Status: model.StatusActive, PaymentProvider: "stripe", UpdatesUntil: &start,
	}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatalf("create: %v", err)
	}
	read := func() time.Time {
		l, err := s.FindLicenseByID(ctx, lic.ID)
		if err != nil || l.UpdatesUntil == nil {
			t.Fatalf("read: %v %v", l, err)
		}
		return *l.UpdatesUntil
	}
	if got := read(); !got.Equal(start) {
		t.Fatalf("start not stored: %v vs %v", got, start)
	}
	var events []ledgerEvent
	var log []string
	refunded := map[string]bool{}
	steps := 1 + rng.Intn(10)
	for i := 0; i < steps; i++ {
		var renewals []string
		for _, e := range events {
			if e.kind == "renew" {
				renewals = append(renewals, e.id)
			}
		}
		p := rng.Intn(100)
		switch {
		case p < 45 || len(renewals) == 0 && p < 75:
			days := []int{30, 100, 365}[rng.Intn(3)]
			r := &model.LicenseRenewal{LicenseID: lic.ID, Days: days,
				StripeCheckoutSessionID: fmt.Sprintf("cs_%s_%d", tag, i), StripePaymentIntentID: fmt.Sprintf("pi_%s_%d", tag, i)}
			if err := store.RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error { return store.ApplyLicenseRenewalIn(ctx, tx, r) }); err != nil {
				t.Fatalf("apply: %v", err)
			}
			events = append(events, ledgerEvent{kind: "renew", days: days, created: r.CreatedAt, id: r.ID})
			log = append(log, fmt.Sprintf("renew %s +%dd -> %v", r.ID[:6], days, read()))
		case p < 75:
			id := renewals[rng.Intn(len(renewals))]
			r := &model.LicenseRenewal{ID: id, LicenseID: lic.ID}
			if err := store.RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error { return store.RevertLicenseRenewalIn(ctx, tx, r) }); err != nil {
				t.Fatalf("revert: %v", err)
			}
			again := refunded[id]
			refunded[id] = true
			events = append(events, ledgerEvent{kind: "refund", id: id})
			log = append(log, fmt.Sprintf("refund %s (again=%v) -> %v", id[:6], again, read()))
		case p < 95:
			cur := read()
			next := cur.Add(time.Duration(rng.Intn(1001)-500)*day + time.Duration(rng.Intn(86400))*time.Second).Truncate(time.Second)
			if rng.Intn(2) == 0 {
				l, err := s.FindLicenseByID(ctx, lic.ID)
				if err != nil {
					t.Fatal(err)
				}
				l.UpdatesUntil = &next
				if err := s.UpdateLicense(ctx, l, "updates_until"); err != nil {
					t.Fatalf("admin update: %v", err)
				}
			} else if ok, err := s.SetLicenseUpdatesUntil(ctx, lic.ID, &cur, &next, false); err != nil || !ok {
				t.Fatalf("admin set: %v %v", ok, err)
			}
			got := read()
			events = append(events, ledgerEvent{kind: "edit", delta: got.Sub(cur)})
			log = append(log, fmt.Sprintf("edit %v -> %v", got.Sub(cur), got))
		default:
			r := &model.LicenseRenewal{LicenseID: lic.ID, Days: 365,
				StripeCheckoutSessionID: fmt.Sprintf("cs_%s_e%d", tag, i), StripePaymentIntentID: fmt.Sprintf("pi_%s_e%d", tag, i)}
			if err := s.RecordRefundedRenewal(ctx, r); err != nil {
				t.Fatalf("early: %v", err)
			}
			events = append(events, ledgerEvent{kind: "early"})
			log = append(log, fmt.Sprintf("early refund record -> %v", read()))
		}
	}
	// The model.
	net := start
	for _, e := range events {
		switch e.kind {
		case "renew":
			if !refunded[e.id] {
				net = model.RenewedUpdatesUntil(&net, e.created, e.days)
			}
		case "edit":
			net = net.Add(e.delta)
		}
	}
	got := read()
	if d := got.Sub(net).Abs(); d > time.Millisecond {
		return fmt.Sprintf("start %v (now %v)\n  %s\n got %v want %v (off by %v)", start, time.Now().Round(time.Second), strings.Join(log, "\n  "), got, net, got.Sub(net))
	}
	return ""
}
