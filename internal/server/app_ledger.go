package server

// Cross-app aggregation for Project K's ledger: the apps' own boundary
// crossings, read from each contracted app's optional
// GET /fog/connect/ledger (docs/connect-contract.md) and merged into
// fog's feed in fog's row shape. Best-effort by construction — an app that
// never implemented the endpoint, is redeploying, or is slow costs the page
// its rows, never the page.

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/store"
)

// AppLedgerPath is the optional contract endpoint an app serves.
const AppLedgerPath = "/fog/connect/ledger"

// appLedgerCallTimeout bounds one app read: the ledger renders on a page,
// and a stalled app must not hold it.
const appLedgerCallTimeout = 3 * time.Second

// appLedgerRow is the row shape the contract asks apps for: fog's own audit
// row (at, actor, action, subject, detail) plus the axis and a plain
// reading, so no translation layer sits between the two ledgers.
type appLedgerRow struct {
	At      string `json:"at"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
	Axis    string `json:"axis"`
	Summary string `json:"summary"`
}

// appLedgerEntries reads every contracted app's ledger, in parallel, and
// returns the rows as ledger entries tagged with the app they came from.
// peerInstance narrows an app's rows to one peer household by its instance
// id — the key an app knows a fog peer by.
func (s *Server) appLedgerEntries(ctx context.Context, peerInstance string, limit int) []ledgerEntry {
	list, err := s.store.AppsWithContract(ContractConnectV1)
	if err != nil {
		return nil
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if peerInstance != "" {
		q.Set("peer", peerInstance)
	}
	var (
		mu  sync.Mutex
		out []ledgerEntry
	)
	s.fanOutApps(ctx, list, appLedgerCallTimeout, func(ctx context.Context, t *appcall.Target) {
		rows := s.readAppLedger(ctx, t, AppLedgerPath+"?"+q.Encode())
		mu.Lock()
		out = append(out, rows...)
		mu.Unlock()
	})
	return out
}

func (s *Server) readAppLedger(ctx context.Context, t *appcall.Target, path string) []ledgerEntry {
	app := &t.App
	raw, err := t.Call(ctx, "GET", path, nil)
	if err != nil {
		// Debug, not Warn: the endpoint is optional, so "app answered 404"
		// is the expected shape of an app that never opted in.
		s.log.Debug("federation: app ledger", "app", app.Name, "err", err)
		return nil
	}
	var payload struct {
		Entries []appLedgerRow `json:"entries"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.log.Debug("federation: app ledger", "app", app.Name, "err", err)
		return nil
	}
	rows := make([]ledgerEntry, 0, len(payload.Entries))
	for _, r := range payload.Entries {
		if r.At == "" || r.Action == "" {
			continue
		}
		cat := r.Axis
		if cat == "" {
			cat = "sharing"
		}
		summary := r.Summary
		if summary == "" {
			summary = r.Action
		}
		actor := r.Actor
		if actor == "" {
			actor = "app:" + app.Name
		}
		rows = append(rows, ledgerEntry{
			AuditEntry: store.AuditEntry{At: r.At, Actor: actor, Action: r.Action, Subject: r.Subject, Detail: r.Detail},
			Category:   cat,
			Summary:    summary,
			Source:     app.Name,
		})
	}
	return rows
}

// mergeLedger interleaves fog's rows with the apps', newest first, and keeps
// the limit. App rows carry no database id; they are keyed by position
// below zero so a list renderer never sees two rows share an id.
func mergeLedger(fog, apps []ledgerEntry, limit int) []ledgerEntry {
	all := make([]ledgerEntry, 0, len(fog)+len(apps))
	all = append(all, fog...)
	all = append(all, apps...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].At > all[j].At })
	if len(all) > limit {
		all = all[:limit]
	}
	for i := range all {
		if all[i].Source != "" {
			all[i].ID = -int64(i + 1)
		}
	}
	return all
}
