package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/computenode"
)

// A machine fog removed, or never paired, keeps its service up and idle,
// waiting for a pairing, rather than exiting for the service manager to
// start it again and again.
func TestRunWaitsForAPairing(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := run(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("run answered %v after %v", err, time.Since(start))
	}
}

func TestUnconfinedIsTheOwnersChoice(t *testing.T) {
	dir := t.TempDir()
	if err := setUnconfined(dir, "on"); !errors.Is(err, computenode.ErrNotPaired) {
		t.Fatalf("unpaired: %v", err)
	}
	(&computenode.Config{Fog: "http://fog", Token: "fognode_x", FogKey: "k", Name: "m"}).Save(dir)
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	err := setUnconfined(dir, "on")
	os.Stdout, os.Stderr = stdout, stderr
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := computenode.LoadConfig(dir)
	if !cfg.Unconfined {
		t.Fatal("not saved")
	}
	if err := setUnconfined(dir, "maybe"); err == nil {
		t.Fatal("took a word that isn't on or off")
	}
}

// fog-node join takes the code from FOG_PAIR_CODE or stdin, never an
// argument, and leaves it in no environment its children inherit.
func TestPairingCodeIsNeverAnArgument(t *testing.T) {
	r, w, _ := os.Pipe()
	w.Close()
	t.Setenv(pairCodeEnv, " ABCD-EFGH ")
	if code, err := pairingCode(r); err != nil || code != "ABCD-EFGH" {
		t.Fatalf("from the environment: %q %v", code, err)
	}
	if _, ok := os.LookupEnv(pairCodeEnv); ok {
		t.Fatal("the code stayed in the environment")
	}
	r2, w2, _ := os.Pipe()
	w2.Write([]byte("WXYZ-2345\n"))
	w2.Close()
	if code, err := pairingCode(r2); err != nil || code != "WXYZ-2345" {
		t.Fatalf("from stdin: %q %v", code, err)
	}
	r3, w3, _ := os.Pipe()
	w3.Close()
	if _, err := pairingCode(r3); err == nil {
		t.Fatal("no code at all was taken")
	}
}
