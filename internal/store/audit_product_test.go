package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// An audit line records the product its entity belongs to, two steps
// away included (a device, an entitlement), so the product filter finds
// it; a line for something already deleted finds it by the product the
// caller set. Another product's lines stay out.
func TestAuditLinesCarryTheirProduct(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	lic := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	other := newLicenseOn(t, s, ctx, "subscription", model.StatusActive, nil)
	act := &model.Activation{LicenseID: lic.ID, Identifier: "pc-" + lic.ID, IdentifierType: "device"}
	if err := s.CreateActivation(ctx, act); err != nil {
		t.Fatal(err)
	}
	ent := &model.Entitlement{PlanID: lic.PlanID, Feature: "exports", ValueType: "quota", Value: "5"}
	if err := s.CreateEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	wh := &model.Webhook{ProductID: lic.ProductID, URL: "https://example.com/hook", Secret: "s", Events: []string{"license.created"}, Active: true}
	if err := s.CreateWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}

	seat := &model.Seat{LicenseID: lic.ID, Email: "seat-" + lic.ID + "@example.com", Role: "member"}
	if _, _, err := s.AddSeatWithinLimit(ctx, seat, 0); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ entity, id, want string }{
		{"seat", seat.ID, lic.ProductID},
		{"product", lic.ProductID, lic.ProductID},
		{"license", lic.ID, lic.ProductID},
		{"plan", lic.PlanID, lic.ProductID},
		{"activation", act.ID, lic.ProductID},
		{"entitlement", ent.ID, lic.ProductID},
		{"webhook", wh.ID, lic.ProductID},
		{"activation", "gone", ""},
		{"session", "s1", ""},
	} {
		if got := s.ProductOfAudited(ctx, c.entity, c.id); got != c.want {
			t.Errorf("ProductOfAudited(%s, %s) = %q, want %q", c.entity, c.id, got, c.want)
		}
	}

	s.Audit(ctx, &model.AuditLog{Entity: "activation", EntityID: act.ID, Action: "created", ActorType: "test"})
	s.Audit(ctx, &model.AuditLog{Entity: "entitlement", EntityID: ent.ID, Action: "updated", ActorType: "test"})
	s.Audit(ctx, &model.AuditLog{Entity: "addon", EntityID: "deleted-addon-" + lic.ID, Action: "deleted", ActorType: "test", ProductID: lic.ProductID})
	s.Audit(ctx, &model.AuditLog{Entity: "license", EntityID: other.ID, Action: "updated", ActorType: "test"})

	var logs []*model.AuditLog
	for range 30 {
		var err error
		if logs, _, err = s.ListAuditLogs(ctx, "", "", lic.ProductID, 0, 50); err != nil {
			t.Fatal(err)
		}
		if len(logs) >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	got := map[string]bool{}
	for _, l := range logs {
		got[l.Entity+":"+l.Action] = true
		if l.ProductID != lic.ProductID {
			t.Errorf("%s %s recorded product %q, want %q", l.Entity, l.Action, l.ProductID, lic.ProductID)
		}
	}
	for _, want := range []string{"activation:created", "entitlement:updated", "addon:deleted"} {
		if !got[want] {
			t.Errorf("the product's audit page misses %s (has %v)", want, got)
		}
	}
	if got["license:updated"] {
		t.Error("another product's line shows on this product's page")
	}
}
