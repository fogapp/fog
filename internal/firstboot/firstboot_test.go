package firstboot

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func write(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConsumeAppliesAndRemoves(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	before := time.Now().UTC().Truncate(time.Second)
	path := write(t, dir, `{
		"username": "matthew",
		"passwordHash": "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"serverName": "fog",
		"claimTokenDigest": "abc123",
		"claimTokenTTL": "1h"
	}`)

	if err := Consume(dir, st, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("firstboot.json not removed after consume")
	}
	if n, _ := st.CountOperators(); n != 1 {
		t.Fatalf("operators = %d, want 1", n)
	}
	// The preseeded account signs in like anyone else: a person with a
	// password credential, not a row in a table of its own.
	p, err := st.PersonByHandle("matthew")
	if err != nil || p.Role != store.PersonRoleOperator {
		t.Fatalf("operator person: %+v %v", p, err)
	}
	creds, _ := st.PersonCredentials(p.ID)
	if len(creds) != 1 || creds[0].Kind != store.CredentialPassword {
		t.Fatalf("credentials = %+v, want one password", creds)
	}
	for key, want := range map[string]string{
		store.SettingSetupDone:   "1",
		store.SettingServerName:  "fog",
		store.SettingClaimDigest: "abc123",
		store.SettingClaimUser:   "matthew",
	} {
		if got, _ := st.GetSetting(key); got != want {
			t.Errorf("setting %s = %q, want %q", key, got, want)
		}
	}
	raw, _ := st.GetSetting(store.SettingClaimExpires)
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil || exp.Before(before.Add(time.Hour)) || exp.After(time.Now().Add(time.Hour)) {
		t.Errorf("claim deadline %q is not an hour from consume", raw)
	}
}

func TestConsumeStampsTTLFromOwnClock(t *testing.T) {
	// The installer stages a duration, not a deadline: its live-boot clock
	// may never have seen NTP, so the absolute stamp is minted here, at
	// consume time, by the installed system's own clock.
	st := testStore(t)
	dir := t.TempDir()
	before := time.Now()
	write(t, dir, `{"username": "mm", "passwordHash": "$argon2id$x",
		"claimTokenDigest": "abc", "claimTokenTTL": "24h"}`)
	if err := Consume(dir, st, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSetting(store.SettingClaimDigest); got != "abc" {
		t.Fatalf("claim digest = %q, want abc", got)
	}
	raw, _ := st.GetSetting(store.SettingClaimExpires)
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("stamped deadline %q: %v", raw, err)
	}
	want := before.Add(24 * time.Hour)
	if expires.Before(want.Add(-time.Minute)) || expires.After(want.Add(time.Hour)) {
		t.Fatalf("deadline %v is not ~24h from consume time %v", expires, before)
	}
}

func TestConsumeDropsBadTTL(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	// A TTL that is present but broken must drop the token — not fall back
	// to the (absent) absolute form, and not fail the whole preseed.
	write(t, dir, `{"username": "mm", "passwordHash": "$argon2id$x",
		"claimTokenDigest": "abc", "claimTokenTTL": "-24h"}`)
	if err := Consume(dir, st, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSetting(store.SettingClaimDigest); got != "" {
		t.Fatal("bad TTL must drop the token")
	}
	if got, _ := st.GetSetting(store.SettingSetupDone); got != "1" {
		t.Fatal("setup must still complete")
	}
}

func TestConsumeMissingFileIsNoop(t *testing.T) {
	if err := Consume(t.TempDir(), testStore(t), slog.Default()); err != nil {
		t.Fatal(err)
	}
}

func TestConsumeRejectsBadFile(t *testing.T) {
	cases := map[string]string{
		"garbage":    `not json`,
		"no user":    `{"passwordHash": "$argon2id$x"}`,
		"plain hash": `{"username": "mm", "passwordHash": "hunter2"}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			dir := t.TempDir()
			path := write(t, dir, content)
			if err := Consume(dir, st, slog.Default()); err == nil {
				t.Fatal("want error")
			}
			if _, err := os.Stat(path + ".rejected"); err != nil {
				t.Fatal("rejected file not set aside")
			}
			if n, _ := st.CountOperators(); n != 0 {
				t.Fatal("bad preseed created an operator")
			}
		})
	}
}

func TestConsumeRejectSetAsideFailureIsNamed(t *testing.T) {
	// When a bad file can be neither set aside nor removed, the error must
	// say so and name the path — the old message claimed the file was kept
	// as .rejected while every subsequent boot silently retried it.
	if os.Geteuid() == 0 {
		t.Skip("directory permissions don't bind root")
	}
	st := testStore(t)
	dir := t.TempDir()
	path := write(t, dir, `not json`)
	if err := os.Chmod(dir, 0o500); err != nil { // no renames, no removes
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := Consume(dir, st, slog.Default())
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("error doesn't tell the operator what to do: %v", err)
	}
	// And it must not have claimed the set-aside happened.
	if strings.Contains(err.Error(), "kept as") {
		t.Fatalf("error claims the file was set aside: %v", err)
	}
}

func TestConsumeRefusesExistingAccount(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreatePerson("existing", "existing", store.PersonRoleOperator); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, dir, `{"username": "intruder", "passwordHash": "$argon2id$x"}`)
	if err := Consume(dir, st, slog.Default()); err == nil {
		t.Fatal("preseed over existing account must fail")
	}
	if n, _ := st.CountOperators(); n != 1 {
		t.Fatal("operator count changed")
	}
}

func TestConsumeDropsBadTokenDeadline(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	write(t, dir, `{"username": "mm", "passwordHash": "$argon2id$x",
		"claimTokenDigest": "abc", "claimTokenTTL": "yesterday-ish"}`)
	if err := Consume(dir, st, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSetting(store.SettingClaimDigest); got != "" {
		t.Fatal("unparseable deadline must drop the token")
	}
	if got, _ := st.GetSetting(store.SettingSetupDone); got != "1" {
		t.Fatal("setup must still complete")
	}
}
