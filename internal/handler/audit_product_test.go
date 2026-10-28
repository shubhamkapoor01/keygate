package handler

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// A deletion's audit line keeps the product the deleted thing belonged
// to, read before the delete, so the product's audit page still shows
// it. A webhook's lines name its target by scheme and host only.
func TestDeletionsKeepTheirProduct(t *testing.T) {
	f := newAPIFixture(t)
	admin := &AdminHandler{Store: f.s}
	hooks := &WebhookAdminHandler{Store: f.s, Webhook: service.NewWebhookService(f.s, slog.Default(), time.Second, 1, true)}

	plan := &model.Plan{ProductID: f.prodA.ID, Name: "Gone", Slug: "gone-" + f.suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := f.s.CreatePlan(f.ctx, plan); err != nil {
		t.Fatal(err)
	}
	ent := &model.Entitlement{PlanID: f.planA.ID, Feature: "gone_" + f.suffix, ValueType: "bool", Value: "true"}
	if err := f.s.CreateEntitlement(f.ctx, ent); err != nil {
		t.Fatal(err)
	}
	addon := &model.Addon{ProductID: f.prodA.ID, Name: "Gone", Slug: "gone-" + f.suffix, Feature: "gone", ValueType: "bool", Value: "true"}
	if err := f.s.CreateAddon(f.ctx, addon); err != nil {
		t.Fatal(err)
	}
	// Made and changed through the API: those lines keep the target too.
	code, out := f.call(hooks.CreateWebhook, http.MethodPost, "/admin/webhooks",
		`{"product_id":"`+f.prodA.ID+`","url":"https://hooks.example.com/t/secret-token","events":["license.created"]}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("create webhook: %d %v", code, out)
	}
	wh := &model.Webhook{ID: out["data"].(map[string]any)["id"].(string)}
	if code, out := f.call(hooks.UpdateWebhook, http.MethodPut, "/admin/webhooks/"+wh.ID, `{"active":false}`, nil, gin.Param{Key: "id", Value: wh.ID}); code != http.StatusOK {
		t.Fatalf("update webhook: %d %v", code, out)
	}

	for _, d := range []struct {
		entity, id string
		fn         gin.HandlerFunc
	}{
		{"plan", plan.ID, admin.DeletePlan},
		{"entitlement", ent.ID, admin.DeleteEntitlement},
		{"addon", addon.ID, admin.DeleteAddon},
		{"webhook", wh.ID, hooks.DeleteWebhook},
	} {
		if code, out := f.call(d.fn, http.MethodDelete, "/admin/x/"+d.id, "", nil, gin.Param{Key: "id", Value: d.id}); code != http.StatusNoContent {
			t.Fatalf("delete %s: %d %v", d.entity, code, out)
		}
		var pid, changes sql.NullString
		for range 30 {
			err := f.s.DB.NewRaw("SELECT product_id, changes::text FROM audit_logs WHERE entity = ? AND entity_id = ? AND action = 'deleted'", d.entity, d.id).Scan(f.ctx, &pid, &changes)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if pid.String != f.prodA.ID {
			t.Errorf("%s deleted: audit product %q, want %q", d.entity, pid.String, f.prodA.ID)
		}
		if d.entity == "webhook" && changes.String != `{"target": "https://hooks.example.com"}` {
			t.Errorf("webhook deleted: changes %s, want the target's scheme and host only", changes.String)
		}
	}
	var lines []string
	if err := f.s.DB.NewRaw("SELECT action || ' ' || changes::text FROM audit_logs WHERE entity = 'webhook' AND entity_id = ? ORDER BY created_at", wh.ID).Scan(f.ctx, &lines); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if strings.Contains(l, "secret") || strings.Contains(l, "/t/") {
			t.Errorf("webhook audit line keeps the URL's path: %s", l)
		}
	}
	if len(lines) != 3 {
		t.Errorf("webhook audit lines %v, want created, updated and deleted", lines)
	}
}
