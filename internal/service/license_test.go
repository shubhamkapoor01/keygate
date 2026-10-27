package service

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
)

func TestAssertUsable(t *testing.T) {
	svc := &LicenseService{}

	now := time.Now()
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)
	wayPast := now.Add(-30 * 24 * time.Hour)

	tests := []struct {
		name    string
		license *model.License
		wantErr bool
		errCode string
	}{
		{
			name:    "active license with future expiry",
			license: &model.License{Status: model.StatusActive, ValidUntil: &future, Plan: &model.Plan{GraceDays: 7}},
			wantErr: false,
		},
		{
			name:    "active license no expiry",
			license: &model.License{Status: model.StatusActive, Plan: &model.Plan{GraceDays: 7}},
			wantErr: false,
		},
		{
			name:    "active license recently expired within grace",
			license: &model.License{Status: model.StatusActive, ValidUntil: &past, Plan: &model.Plan{GraceDays: 7}},
			wantErr: false,
		},
		{
			name:    "active license expired beyond grace",
			license: &model.License{Status: model.StatusActive, ValidUntil: &wayPast, Plan: &model.Plan{GraceDays: 7}},
			wantErr: true,
			errCode: "LICENSE_EXPIRED",
		},
		{
			name:    "trialing license",
			license: &model.License{Status: model.StatusTrialing, Plan: &model.Plan{GraceDays: 7}},
			wantErr: false,
		},
		{
			name:    "canceled license within paid period",
			license: &model.License{Status: model.StatusCanceled, ValidUntil: &future},
			wantErr: false,
		},
		{
			name:    "canceled license past paid period",
			license: &model.License{Status: model.StatusCanceled, ValidUntil: &past},
			wantErr: true,
			errCode: "LICENSE_CANCELED",
		},
		{
			name:    "suspended license",
			license: &model.License{Status: model.StatusSuspended},
			wantErr: true,
			errCode: "LICENSE_SUSPENDED",
		},
		{
			name:    "revoked license",
			license: &model.License{Status: model.StatusRevoked},
			wantErr: true,
			errCode: "LICENSE_REVOKED",
		},
		{
			name:    "expired license",
			license: &model.License{Status: model.StatusExpired},
			wantErr: true,
			errCode: "LICENSE_EXPIRED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.assertUsable(tt.license)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				// Check error contains expected code
				if tt.errCode != "" && !containsCode(err, tt.errCode) {
					t.Fatalf("expected error code %s, got %v", tt.errCode, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
			}
		})
	}
}

func containsCode(err error, code string) bool {
	return err != nil && containsStr(err.Error(), code)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMaxActivations(t *testing.T) {
	svc := &LicenseService{}

	tests := []struct {
		name string
		lic  *model.License
		want int
	}{
		{"with plan", &model.License{Plan: &model.Plan{MaxActivations: 5}}, 5},
		{"without plan", &model.License{}, 3},
		{"nil plan", &model.License{Plan: nil}, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.maxActivations(tt.lic)
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGraceDays(t *testing.T) {
	svc := &LicenseService{}

	tests := []struct {
		name string
		lic  *model.License
		want int
	}{
		{"with plan", &model.License{Plan: &model.Plan{GraceDays: 14}}, 14},
		{"without plan", &model.License{}, 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.graceDays(tt.lic)
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEntitlements(t *testing.T) {
	lic := &model.License{
		Plan: &model.Plan{
			Entitlements: []*model.Entitlement{
				{Feature: "export", ValueType: "bool", Value: "true"},
				{Feature: "sso", ValueType: "bool", Value: "false"},
				{Feature: "max_users", ValueType: "int", Value: "50"},
				{Feature: "sla", ValueType: "string", Value: "99.9%"},
			},
		},
	}

	ents, err := LicenseFeatures(context.Background(), nil, lic)
	if err != nil {
		t.Fatal(err)
	}
	features := featureValues(ents)

	if features["export"] != true {
		t.Error("export should be true")
	}
	if features["sso"] != false {
		t.Error("sso should be false")
	}
	if features["max_users"] != "50" {
		t.Errorf("max_users should be '50', got %v", features["max_users"])
	}
	if features["sla"] != "99.9%" {
		t.Errorf("sla should be '99.9%%', got %v", features["sla"])
	}

	nilEnts, _ := LicenseFeatures(context.Background(), nil, &model.License{})
	emptyFeatures := featureValues(nilEnts)
	if len(emptyFeatures) != 0 {
		t.Error("nil plan should return empty features")
	}
}

// A signed token must never outlive the licence it was issued for.
// With a flat 7-day TTL a plan with a short grace period handed out
// tokens that still verified offline after the licence was dead.
func TestSignTokenClampsToLicenceDeadline(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	svc := &LicenseService{signingKey: priv}

	now := time.Now()
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	tests := []struct {
		name       string
		status     string
		validUntil *time.Time
		graceDays  int
		ttlDays    int // plan token_ttl_days; 0 = default
		wantExp    time.Time
		wantVun    bool
	}{
		{
			// The case from the report: grace shorter than the TTL.
			name: "short grace clamps the token", validUntil: at(24 * time.Hour), graceDays: 0,
			wantExp: now.Add(24 * time.Hour), wantVun: true,
		},
		{
			name: "grace inside the TTL still clamps", validUntil: at(24 * time.Hour), graceDays: 2,
			wantExp: now.Add(72 * time.Hour), wantVun: true,
		},
		{
			// Deadline beyond the TTL — the TTL wins, because exp is a
			// check-in interval, not the licence term.
			name: "distant expiry leaves the TTL alone", validUntil: at(365 * 24 * time.Hour), graceDays: 7,
			wantExp: now.Add(tokenTTL), wantVun: true,
		},
		{
			name: "perpetual licence keeps the full TTL", validUntil: nil, graceDays: 7,
			wantExp: now.Add(tokenTTL), wantVun: false,
		},
		{
			// A canceled licence runs out at ValidUntil — assertUsable
			// gives it no grace, so neither may its token.
			name: "canceled licence gets no grace", status: model.StatusCanceled,
			validUntil: at(24 * time.Hour), graceDays: 7,
			wantExp: now.Add(24 * time.Hour), wantVun: true,
		},
		{
			// A plan can lengthen the check-in interval for clients
			// that stay offline a long time.
			name: "plan TTL replaces the default", validUntil: nil, graceDays: 7, ttlDays: 30,
			wantExp: now.Add(30 * 24 * time.Hour), wantVun: false,
		},
		{
			// ...but the licence deadline still wins over a long plan TTL.
			name: "plan TTL is still clamped", validUntil: at(24 * time.Hour), graceDays: 2, ttlDays: 30,
			wantExp: now.Add(72 * time.Hour), wantVun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == "" {
				status = model.StatusActive
			}
			lic := &model.License{
				ID: "lic", ProductID: "prod", PlanID: "plan", Status: status,
				ValidUntil: tt.validUntil,
				Plan:       &model.Plan{GraceDays: tt.graceDays, TokenTTLDays: tt.ttlDays},
			}
			raw, _, err := svc.signToken(context.Background(), lic, "device-1")
			if err != nil {
				t.Fatalf("signToken: %v", err)
			}
			tok, err := license.Verify(raw, license.PublicKey(priv))
			if err != nil {
				t.Fatalf("verify: %v", err)
			}

			if diff := tok.ExpiresAt - tt.wantExp.Unix(); diff > 1 || diff < -1 {
				t.Errorf("exp = %d, want ~%d (off by %ds)", tok.ExpiresAt, tt.wantExp.Unix(), diff)
			}
			if tt.wantVun {
				if tok.ValidUntil != tt.validUntil.Unix() {
					t.Errorf("vun = %d, want %d", tok.ValidUntil, tt.validUntil.Unix())
				}
			} else if tok.ValidUntil != 0 {
				t.Errorf("vun = %d, want 0 for a perpetual licence", tok.ValidUntil)
			}

			// Whatever the clamp picks, the token must not outlive the
			// licence — and the grace it reports is the grace it used,
			// so a client recomputing vun+grc lands in the same place.
			if tok.GraceDays != 0 && status == model.StatusCanceled {
				t.Errorf("grc = %d, want 0 for a canceled licence", tok.GraceDays)
			}
			if tt.validUntil != nil {
				deadline := tt.validUntil.Add(time.Duration(tok.GraceDays) * 24 * time.Hour).Unix()
				if tok.ExpiresAt > deadline {
					t.Errorf("token outlives the licence: exp %d > deadline %d", tok.ExpiresAt, deadline)
				}
			}
		})
	}
}

// The grace period appears in three places — the lifecycle check, the
// signed token and the verify envelope — and a client that reads a
// different number from any of them is told the licence lives longer
// than the server will allow.
func TestEffectiveGraceDays(t *testing.T) {
	svc := &LicenseService{}
	plan := &model.Plan{GraceDays: 7}

	for _, status := range []string{model.StatusActive, model.StatusTrialing, model.StatusPastDue} {
		lic := &model.License{Status: status, Plan: plan, StripeSubscriptionID: "sub_test"}
		if got := svc.effectiveGraceDays(lic); got != 7 {
			t.Errorf("%s: grace = %d, want the plan's 7", status, got)
		}
	}

	// Canceled is paid up to ValidUntil and stops there — assertUsable
	// gives it no grace, so nothing else may either.
	lic := &model.License{Status: model.StatusCanceled, Plan: plan}
	if got := svc.effectiveGraceDays(lic); got != 0 {
		t.Errorf("canceled: grace = %d, want 0", got)
	}

	lic = &model.License{Status: model.StatusTrialing, Plan: plan}
	if got := svc.effectiveGraceDays(lic); got != 0 {
		t.Errorf("trialing, not billed by Stripe: grace = %d, want 0", got)
	}
}

// LicenseUsable is the activation rule for callers outside the service,
// and gives the same answers.
func TestLicenseUsableMatchesActivation(t *testing.T) {
	past := time.Now().Add(-48 * time.Hour)
	future := time.Now().Add(48 * time.Hour)
	plan := &model.Plan{GraceDays: 7}
	for _, lic := range []*model.License{
		{Status: model.StatusActive, ValidUntil: &future, Plan: plan},
		{Status: model.StatusActive, ValidUntil: &past, Plan: plan}, // within grace
		{Status: model.StatusPastDue, ValidUntil: &future, Plan: plan},
		{Status: model.StatusCanceled, ValidUntil: &future, Plan: plan},
		{Status: model.StatusCanceled, ValidUntil: &past, Plan: plan},
		{Status: model.StatusSuspended, ValidUntil: &future, Plan: plan},
		{Status: model.StatusRevoked, ValidUntil: &future, Plan: plan},
		{Status: model.StatusExpired, ValidUntil: &past, Plan: plan},
	} {
		svc := &LicenseService{}
		want, got := svc.assertUsable(lic), LicenseUsable(lic)
		if (want == nil) != (got == nil) || (want != nil && want.Error() != got.Error()) {
			t.Errorf("%s until %v: LicenseUsable=%v, activation=%v", lic.Status, lic.ValidUntil, got, want)
		}
	}
	if LicenseUsable(&model.License{Status: model.StatusRevoked}) == nil {
		t.Fatal("revoked licence reported usable")
	}
}
