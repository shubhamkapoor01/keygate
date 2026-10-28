package store_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Trial conversion counts the trials whose upgrade to a paid plan was
// applied, whatever the licence's state since, out of every licence
// that was a trial: still on its trial plan, converted, or moved off it
// (to its fallback plan, say).
func TestGrowthMetricsTrialConversion(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	product := func(name string) *model.Product {
		p := &model.Product{Name: name, Slug: strings.ToLower(name) + "-" + suffix, Type: "desktop"}
		if err := s.CreateProduct(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plan := func(p *model.Product, slug, kind string) *model.Plan {
		pl := &model.Plan{ProductID: p.ID, Name: slug, Slug: slug + "-" + suffix, LicenseType: kind, LicenseModel: "standard", Active: true}
		if kind == "trial" {
			pl.TrialDays = 14
		}
		if err := s.CreatePlan(ctx, pl); err != nil {
			t.Fatal(err)
		}
		return pl
	}
	prod := product("Growth")
	trial, paid, free := plan(prod, "trial", "trial"), plan(prod, "paid", "subscription"), plan(prod, "free", "perpetual")
	n := 0
	licence := func(p *model.Product, pl *model.Plan) *model.License {
		n++
		l := &model.License{ProductID: p.ID, PlanID: pl.ID, Email: "growth-" + suffix + "@example.com",
			LicenseKey: "KG-GR-" + suffix + "-" + string(rune('a'+n)), Status: model.StatusTrialing}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	upgrade := func(l *model.License, state string) {
		u := &model.LicenseUpgrade{LicenseID: l.ID, FromPlanID: trial.ID, ToPlanID: paid.ID, State: state,
			StripeCheckoutSessionID: "cs_" + l.LicenseKey}
		if err := store.CreateLicenseUpgradeIn(ctx, s.DB, u); err != nil {
			t.Fatal(err)
		}
	}
	move := func(l *model.License, to *model.Plan, status string) {
		l.PlanID, l.Status = to.ID, status
		if err := s.UpdateLicense(ctx, l, "plan_id", "status"); err != nil {
			t.Fatal(err)
		}
		// Written directly: Store.Audit writes in the background.
		if _, err := s.DB.NewInsert().Model(&model.AuditLog{ID: uuid.NewString(), Entity: "license", EntityID: l.ID, Action: "plan_changed",
			ActorType: "system", Changes: map[string]any{"old_plan_id": trial.ID, "new_plan_id": to.ID}}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := func(step string, total, converted int, rate float64) {
		t.Helper()
		gm, err := s.GetGrowthMetrics(ctx, prod.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gm.TotalTrials != total || gm.ConvertedTrials != converted || math.Abs(gm.TrialConversion-rate) > 1e-9 {
			t.Errorf("%s: trials %d, converted %d, rate %.1f; want %d, %d, %.1f", step,
				gm.TotalTrials, gm.ConvertedTrials, gm.TrialConversion, total, converted, rate)
		}
	}

	a := licence(prod, trial)
	want("in trial", 1, 0, 0)

	upgrade(a, model.UpgradeApplied)
	move(a, paid, model.StatusActive)
	want("upgraded", 1, 1, 100)

	a.Status = model.StatusPastDue
	if err := s.UpdateLicense(ctx, a, "status"); err != nil {
		t.Fatal(err)
	}
	want("converted, then past due", 1, 1, 100)

	b := licence(prod, trial)
	move(b, free, model.StatusActive) // the trial ended onto its fallback plan
	want("one moved to its fallback", 2, 1, 50)

	c := licence(prod, trial)
	upgrade(c, model.UpgradeReverted) // refunded and undone: back on the trial plan
	want("an upgrade undone", 3, 1, 100.0/3)

	// A trial plan sold through Stripe: its first charge makes the
	// licence active on the same plan, with no upgrade or plan change.
	d := licence(prod, trial)
	d.Status = model.StatusActive
	if err := s.UpdateLicense(ctx, d, "status"); err != nil {
		t.Fatal(err)
	}
	want("a Stripe-billed trial charged", 4, 2, 50)

	// Another product's trials are its own.
	other := product("Other")
	upgrade(licence(other, plan(other, "trial2", "trial")), model.UpgradeApplied)
	want("another product's trial", 4, 2, 50)
}
