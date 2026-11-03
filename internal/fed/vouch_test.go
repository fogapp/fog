package fed

import (
	"testing"
	"time"
)

func TestVouchRoundTrip(t *testing.T) {
	st, vlt := testDeps(t)
	minter, err := EnsureIdentity(st, vlt)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	v := Vouch{
		From: minter.InstanceID, To: "01JPEEROTHER00000000000000", App: "hook",
		FromApp: "https://hook.a.house", FromKey: "HOOKKEY", Nonce: "n1", IssuedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(VouchTTL).Format(time.RFC3339),
	}
	token := EncodeVouch(v, SignVouch(minter, v))

	got, sig, err := DecodeVouch(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != v {
		t.Fatalf("round trip changed the vouch:\n got %+v\nwant %+v", got, v)
	}
	if err := VerifyVouch(minter.Public, got, sig, now); err != nil {
		t.Fatalf("verify a good vouch: %v", err)
	}

	// Expired.
	if err := VerifyVouch(minter.Public, got, sig, now.Add(VouchTTL+time.Minute)); err == nil {
		t.Fatal("an expired vouch should not verify")
	}
	// Tampered field.
	tampered := got
	tampered.FromApp = "https://evil.house"
	if err := VerifyVouch(minter.Public, tampered, sig, now); err == nil {
		t.Fatal("a tampered vouch should not verify")
	}
	// Wrong key.
	other, err := EnsureIdentity(testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyVouch(other.Public, got, sig, now); err == nil {
		t.Fatal("a vouch must not verify against another key")
	}
}
