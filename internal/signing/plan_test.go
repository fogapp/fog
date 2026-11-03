package signing

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// published is the keys verifiers should hold at t, oldest first.
func published(ring []Key, t time.Time) []Key {
	var out []Key
	for _, k := range ring {
		if k.Published(t) {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SignFrom.Before(out[j].SignFrom) })
	return out
}

// apply runs a plan against a ring the way Keyring does, naming new keys.
func apply(ring []Key, plan []Action, seq *int) []Key {
	drop := map[string]bool{}
	for _, a := range plan {
		if a.Op == Drop {
			drop[a.Key.KID] = true
		}
	}
	var out []Key
	for _, k := range ring {
		if !drop[k.KID] {
			out = append(out, k)
		}
	}
	for _, a := range plan {
		if a.Op == Create {
			*seq++
			k := a.Key
			k.KID = fmt.Sprintf("k%d", *seq)
			out = append(out, k)
		}
	}
	return out
}

func TestPlanBootstrapsAKeyThatSignsNow(t *testing.T) {
	plan := Plan(nil, t0)
	if len(plan) != 1 || plan[0].Op != Create {
		t.Fatalf("plan = %+v", plan)
	}
	k := plan[0].Key
	if !k.Signs(t0) || !k.Published(t0) {
		t.Fatalf("bootstrap key does not sign and publish at once: %+v", k)
	}
}

func TestPlanIsQuietMidLife(t *testing.T) {
	seq := 0
	ring := apply(nil, Plan(nil, t0), &seq)
	if plan := Plan(ring, t0.Add(Life/2)); len(plan) != 0 {
		t.Fatalf("mid-life plan = %+v", plan)
	}
}

func TestPlanPublishesSuccessorLeadAhead(t *testing.T) {
	seq := 0
	ring := apply(nil, Plan(nil, t0), &seq)
	at := t0.Add(Life - Lead)
	plan := Plan(ring, at)
	if len(plan) != 1 || plan[0].Op != Create {
		t.Fatalf("plan = %+v", plan)
	}
	next := plan[0].Key
	if !next.SignFrom.Equal(ring[0].SignUntil) {
		t.Fatalf("successor starts %v, current ends %v", next.SignFrom, ring[0].SignUntil)
	}
	if got := next.SignFrom.Sub(next.PublishAt); got != Lead {
		t.Fatalf("successor published %v ahead, want %v", got, Lead)
	}
	ring = apply(ring, plan, &seq)
	if plan := Plan(ring, at.Add(time.Hour)); len(plan) != 0 {
		t.Fatalf("second plan in the lead window = %+v", plan)
	}
}

func TestRetireReplacesAtOnceAndUnpublishes(t *testing.T) {
	seq := 0
	ring := apply(nil, Plan(nil, t0), &seq)
	now := t0.Add(10 * 24 * time.Hour)
	ring[0] = Retire(ring[0], now)
	if published(ring, now) != nil {
		t.Fatal("a retired key is still published")
	}
	ring = apply(ring, Plan(ring, now), &seq)
	cur, ok := Current(ring, now)
	if !ok || cur.KID == "k1" {
		t.Fatalf("no replacement after retirement: %+v", ring)
	}
}

// TestPlanInvariants drives the planner across five years in uneven steps —
// including gaps far longer than Lead, as when fogd is off for a month — and
// checks after every step that:
//   - exactly one key signs;
//   - every key that signed within the last Tail is still published, so no
//     token fog could still be holding stops verifying early;
//   - nothing past its unpublish time is kept;
//   - a second plan at the same instant does nothing (plans converge).
func TestPlanInvariants(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed))
		seq := 0
		var ring []Key
		lastSigner := map[string]time.Time{}
		now := t0
		for now.Before(t0.Add(5 * 365 * 24 * time.Hour)) {
			ring = apply(ring, Plan(ring, now), &seq)
			if again := Plan(ring, now); len(again) != 0 {
				t.Fatalf("seed %d at %v: plan did not converge: %+v", seed, now, again)
			}
			signing := 0
			for _, k := range ring {
				if k.Signs(now) {
					signing++
				}
				if !now.Before(k.UnpublishAt) {
					t.Fatalf("seed %d at %v: kept %s past unpublish", seed, now, k.KID)
				}
			}
			cur, _ := Current(ring, now)
			if signing < 1 {
				t.Fatalf("seed %d at %v: no key signs", seed, now)
			}
			lastSigner[cur.KID] = now
			for kid, at := range lastSigner {
				if now.Sub(at) >= Tail {
					continue
				}
				found := false
				for _, k := range published(ring, now) {
					found = found || k.KID == kid
				}
				if !found {
					t.Fatalf("seed %d at %v: %s signed %v ago and is unpublished", seed, now, kid, now.Sub(at))
				}
			}
			// Mostly hours, sometimes days, occasionally a month away.
			var step time.Duration
			switch r := rng.IntN(100); {
			case r < 70:
				step = time.Duration(rng.IntN(12)+1) * time.Hour
			case r < 97:
				step = time.Duration(rng.IntN(5)+1) * 24 * time.Hour
			default:
				step = time.Duration(rng.IntN(40)+20) * 24 * time.Hour
			}
			now = now.Add(step)
		}
	}
}
