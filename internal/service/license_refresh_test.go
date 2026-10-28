package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// countingFailures counts the failures the service records against an
// address, as the brute-force guard would.
type countingFailures struct{ failures int }

func (f *countingFailures) RecordFailure(string)                   { f.failures++ }
func (f *countingFailures) RecordSuccess(string)                   {}
func (f *countingFailures) IsBlocked(string) (bool, time.Duration) { return false, 0 }

type refreshFixture struct {
	t        *testing.T
	ctx      context.Context
	s        *store.Store
	ls       *LicenseService
	priv     ed25519.PrivateKey
	failures *countingFailures
	prod     *model.Product
	plan     *model.Plan
	suffix   string
	sequence int
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	f := &refreshFixture{t: t, ctx: context.Background(), s: s, failures: &countingFailures{},
		suffix: time.Now().Format("150405.000000")}
	_, f.priv, _ = ed25519.GenerateKey(rand.Reader)
	f.ls = NewLicenseService(s, f.priv, slog.Default(), f.failures, nil)
	f.prod = &model.Product{Name: "Refresh app", Slug: "refresh-" + f.suffix, Type: "hybrid"}
	if err := s.CreateProduct(f.ctx, f.prod); err != nil {
		t.Fatal(err)
	}
	f.plan = &model.Plan{ProductID: f.prod.ID, Name: "Team", Slug: "refresh-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 5, MaxSeats: 5, Active: true}
	if err := s.CreatePlan(f.ctx, f.plan); err != nil {
		t.Fatal(err)
	}
	return f
}

// license is an active licence with one device activated on its key; it
// returns the key and the token that activation handed out.
func (f *refreshFixture) license() (*model.License, string, string) {
	f.t.Helper()
	f.sequence++
	key := "KG-REFRESH-" + strconv.Itoa(f.sequence) + "-" + f.suffix
	lic := &model.License{ProductID: f.prod.ID, PlanID: f.plan.ID, Email: "refresh-" + strconv.Itoa(f.sequence) + "-" + f.suffix + "@example.com",
		LicenseKey: key, Status: model.StatusActive}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		f.t.Fatal(err)
	}
	return lic, key, f.activate(key, "pc")
}

func (f *refreshFixture) activate(key, device string) string {
	f.t.Helper()
	res, err := f.ls.Activate(f.ctx, ActivateInput{LicenseKey: key, Identifier: device})
	if err != nil {
		f.t.Fatalf("activate %s: %v", device, err)
	}
	return res.Token
}

// mint signs a token as the server would have at issued.
func (f *refreshFixture) mint(lic *model.License, device string, issued time.Time, priv ed25519.PrivateKey) string {
	f.t.Helper()
	tok, err := license.Sign(&license.VerifyToken{LicenseID: lic.ID, ProductID: lic.ProductID, PlanID: lic.PlanID,
		Status: lic.Status, Identifier: device, IssuedAt: issued.Unix(), ExpiresAt: issued.Add(7 * 24 * time.Hour).Unix()}, priv)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func (f *refreshFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.DB.NewRaw(query, args...).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func isCode(err error, code string) bool {
	var ae *apperr.AppError
	return errors.As(err, &ae) && ae.Code == code
}

// A device's token buys a fresh one without the key: same licence and
// device, a new signature and a lifetime that starts again.
func TestRefresh_IssuesAFreshToken(t *testing.T) {
	f := newRefreshFixture(t)
	lic, _, old := f.license()
	oldTok, err := license.Verify(old, f.ls.SigningPublicKey())
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ls.Refresh(f.ctx, RefreshInput{Token: old, Identifier: "pc"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if res.Token == "" || res.Token == old {
		t.Fatal("refresh returned no new token")
	}
	got, err := license.Verify(res.Token, f.ls.SigningPublicKey())
	if err != nil {
		t.Fatalf("refreshed token does not verify: %v", err)
	}
	if got.LicenseID != lic.ID || got.Identifier != "pc" || got.ExpiresAt < oldTok.ExpiresAt {
		t.Fatalf("refreshed token is for %s/%s until %d, want %s/pc until at least %d",
			got.LicenseID, got.Identifier, got.ExpiresAt, lic.ID, oldTok.ExpiresAt)
	}
	if res.Status != model.StatusActive || res.PlanID != f.plan.ID {
		t.Fatalf("refresh answered %s on %s, want verify's answer", res.Status, res.PlanID)
	}
	// And the refreshed token refreshes in turn.
	if _, err := f.ls.Refresh(f.ctx, RefreshInput{Token: res.Token, Identifier: "pc"}); err != nil {
		t.Fatalf("refresh with the refreshed token: %v", err)
	}
}

// Everything that stops a key from verifying a device stops its tokens
// from refreshing, and a token never outlives the activation it was
// issued for. Each refusal answers as an unknown key does and counts
// against the address.
func TestRefresh_Refusals(t *testing.T) {
	f := newRefreshFixture(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	cases := []struct {
		name  string
		token func() (token, device string)
	}{
		{"expired token", func() (string, string) {
			lic, _, _ := f.license()
			return f.mint(lic, "pc", time.Now().Add(-8*24*time.Hour), f.priv), "pc"
		}},
		{"tampered token", func() (string, string) {
			_, _, tok := f.license()
			b := []byte(tok)
			b[3] ^= 1
			return string(b), "pc"
		}},
		{"signed by another key", func() (string, string) {
			lic, _, _ := f.license()
			return f.mint(lic, "pc", time.Now(), otherPriv), "pc"
		}},
		{"another device", func() (string, string) {
			_, key, tok := f.license()
			f.activate(key, "laptop")
			return tok, "laptop"
		}},
		{"device deactivated", func() (string, string) {
			_, key, tok := f.license()
			if err := f.ls.Deactivate(f.ctx, DeactivateInput{LicenseKey: key, Identifier: "pc"}); err != nil {
				f.t.Fatal(err)
			}
			return tok, "pc"
		}},
		{"key rotated", func() (string, string) {
			lic, _, tok := f.license()
			if _, _, err := f.s.RotateLicenseKey(f.ctx, lic.ID); err != nil {
				f.t.Fatal(err)
			}
			return tok, "pc"
		}},
		{"key rotated and the device activated on the new key", func() (string, string) {
			lic, _, _ := f.license()
			old := f.mint(lic, "pc", time.Now().Add(-10*time.Second), f.priv)
			newKey, _, err := f.s.RotateLicenseKey(f.ctx, lic.ID)
			if err != nil {
				f.t.Fatal(err)
			}
			f.activate(newKey, "pc")
			return old, "pc"
		}},
		{"device released and activated again", func() (string, string) {
			lic, key, _ := f.license()
			old := f.mint(lic, "pc", time.Now().Add(-10*time.Second), f.priv)
			if err := f.ls.Deactivate(f.ctx, DeactivateInput{LicenseKey: key, Identifier: "pc"}); err != nil {
				f.t.Fatal(err)
			}
			f.activate(key, "pc")
			return old, "pc"
		}},
		{"licence revoked", func() (string, string) {
			lic, _, tok := f.license()
			f.exec("UPDATE licenses SET status = 'revoked' WHERE id = ?", lic.ID)
			return tok, "pc"
		}},
		{"licence expired", func() (string, string) {
			lic, _, tok := f.license()
			f.exec("UPDATE licenses SET status = 'expired' WHERE id = ?", lic.ID)
			return tok, "pc"
		}},
		{"licence past its end and grace", func() (string, string) {
			lic, _, tok := f.license()
			f.exec("UPDATE licenses SET valid_until = now() - interval '30 days' WHERE id = ?", lic.ID)
			return tok, "pc"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok, device := c.token()
			before := f.failures.failures
			if _, err := f.ls.Refresh(f.ctx, RefreshInput{Token: tok, Identifier: device}); !isCode(err, "LICENSE_NOT_FOUND") {
				t.Fatalf("refresh = %v, want LICENSE_NOT_FOUND", err)
			}
			if f.failures.failures != before+1 {
				t.Fatalf("%d failures recorded, want 1", f.failures.failures-before)
			}
		})
	}
}

// The token the activation after a release hands out refreshes; only
// the ones from before it stop.
func TestRefresh_NewActivationsTokenWorks(t *testing.T) {
	f := newRefreshFixture(t)
	_, key, _ := f.license()
	if err := f.ls.Deactivate(f.ctx, DeactivateInput{LicenseKey: key, Identifier: "pc"}); err != nil {
		t.Fatal(err)
	}
	tok := f.activate(key, "pc")
	if _, err := f.ls.Refresh(f.ctx, RefreshInput{Token: tok, Identifier: "pc"}); err != nil {
		t.Fatalf("refresh with the new activation's token: %v", err)
	}
}

// A floating plan answers refresh as it answers verify.
func TestRefresh_FloatingPlan(t *testing.T) {
	f := newRefreshFixture(t)
	_, _, tok := f.license()
	f.exec("UPDATE plans SET license_model = 'floating', floating_timeout = 30 WHERE id = ?", f.plan.ID)
	if _, err := f.ls.Refresh(f.ctx, RefreshInput{Token: tok, Identifier: "pc"}); !isCode(err, "FLOATING_LICENSE") {
		t.Fatalf("refresh on a floating plan = %v, want FLOATING_LICENSE", err)
	}
}

// Forged tokens lock an address out as unknown keys do, and the lockout
// covers verify too. Other addresses are not affected.
func TestRefresh_ForgedTokensLockTheAddressOut(t *testing.T) {
	f := newRefreshFixture(t)
	lic, key, tok := f.license()
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	forged := f.mint(lic, "pc", time.Now(), otherPriv)
	ls := NewLicenseService(f.s, f.priv, slog.Default(), middleware.NewBruteForceProtection(3, time.Minute, time.Hour, time.Minute), nil)
	refresh := func(tok, ip string) error {
		_, err := ls.Refresh(f.ctx, RefreshInput{Token: tok, Identifier: "pc", IPAddress: ip})
		return err
	}

	for i := 1; i <= 2; i++ {
		if err := refresh(forged, "203.0.113.7"); !isCode(err, "LICENSE_NOT_FOUND") {
			t.Fatalf("forged refresh %d = %v, want LICENSE_NOT_FOUND", i, err)
		}
	}
	if err := refresh(forged, "203.0.113.7"); !isCode(err, "LOCKED_OUT") {
		t.Fatalf("third forged refresh = %v, want LOCKED_OUT", err)
	}
	if err := refresh(tok, "203.0.113.7"); !isCode(err, "LOCKED_OUT") {
		t.Fatalf("genuine refresh from the locked address = %v, want LOCKED_OUT", err)
	}
	if _, err := ls.Verify(f.ctx, VerifyInput{LicenseKey: key, Identifier: "pc", IPAddress: "203.0.113.7"}); !isCode(err, "LOCKED_OUT") {
		t.Fatalf("verify from the locked address = %v, want LOCKED_OUT", err)
	}
	if err := refresh(tok, "198.51.100.9"); err != nil {
		t.Fatalf("refresh from another address: %v", err)
	}
}

// A member's device refreshes as the member's key verifies it: paused,
// it stops, room again, it works, and removed, it stops for good. The
// owner's devices are not affected.
func TestRefresh_MemberDevice(t *testing.T) {
	f := newRefreshFixture(t)
	lic, ownerKey, ownerTok := f.license()
	plain, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	seat := &model.Seat{LicenseID: lic.ID, Email: "alice-" + f.suffix + "@example.com", Role: "member",
		InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := f.s.AddSeatWithinLimit(f.ctx, seat, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.AcceptSeatInvite(f.ctx, plain); err != nil {
		t.Fatal(err)
	}
	seat, _ = f.s.FindSeatByID(f.ctx, seat.ID)
	aliceKey, err := f.s.MemberKey(f.ctx, seat)
	if err != nil || aliceKey == "" {
		t.Fatalf("member key %q: %v", aliceKey, err)
	}
	aliceTok := f.activate(aliceKey, "alice-pc")
	refresh := func(tok, device string) error {
		_, err := f.ls.Refresh(f.ctx, RefreshInput{Token: tok, Identifier: device})
		return err
	}
	if err := refresh(aliceTok, "alice-pc"); err != nil {
		t.Fatalf("refresh alice's device: %v", err)
	}

	f.exec("UPDATE plans SET max_seats = 1 WHERE id = ?", f.plan.ID)
	if err := refresh(aliceTok, "alice-pc"); !isCode(err, "LICENSE_NOT_FOUND") {
		t.Fatalf("refresh while alice is paused = %v, want LICENSE_NOT_FOUND", err)
	}
	if err := refresh(ownerTok, "pc"); err != nil {
		t.Fatalf("the owner stopped with alice paused: %v", err)
	}
	f.exec("UPDATE plans SET max_seats = 5 WHERE id = ?", f.plan.ID)
	if err := refresh(aliceTok, "alice-pc"); err != nil {
		t.Fatalf("refresh once there is room again: %v", err)
	}

	seats := NewSeatService(f.s, NewWebhookService(f.s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	if err := seats.RemoveSeat(f.ctx, ownerKey, seat.ID, "", "", lic.Email); err != nil {
		t.Fatal(err)
	}
	if err := refresh(aliceTok, "alice-pc"); !isCode(err, "LICENSE_NOT_FOUND") {
		t.Fatalf("refresh after alice was removed = %v, want LICENSE_NOT_FOUND", err)
	}
	if err := refresh(ownerTok, "pc"); err != nil {
		t.Fatalf("the owner stopped with alice removed: %v", err)
	}
}
