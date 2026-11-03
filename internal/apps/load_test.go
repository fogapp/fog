package apps

import (
	"path/filepath"
	"reflect"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

// LatestSpecs is LatestSpec in one query, and must answer identically per
// app: the newest revision decoded, and no entry exactly where LatestSpec
// errors (no revisions, unparseable JSON).
func TestLatestSpecsMatchesLatestSpec(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mk := func(name string, revSpecs ...string) int64 {
		t.Helper()
		app, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range revSpecs {
			if _, err := st.CreateRevision(app.ID, s, "", "test"); err != nil {
				t.Fatal(err)
			}
		}
		return app.ID
	}
	multi := mk("multi",
		`{"source":{"type":"image","image":"a:1"}}`,
		`{"source":{"type":"image","image":"a:2"},"routes":[{"port":80}]}`)
	bare := mk("bare")                    // no revisions at all
	corrupt := mk("corrupt", `{not-json`) // stored JSON gone bad

	batch, err := LatestSpecs(st)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]int64{"multi": multi, "bare": bare, "corrupt": corrupt} {
		single, serr := LatestSpec(st, id)
		got, ok := batch[id]
		if serr != nil {
			if ok {
				t.Errorf("%s: LatestSpec errors (%v) but the batch carries an entry: %+v", name, serr, got)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(got, single) {
			t.Errorf("%s: batch = %+v, LatestSpec = %+v", name, got, single)
		}
	}
	// The batch must return the NEWEST revision, not just any.
	if spec := batch[multi]; spec == nil || spec.Source.Image != "a:2" {
		t.Errorf("multi: batch returned %+v, want the rev-2 spec", batch[multi])
	}
	if len(batch) != 1 {
		t.Errorf("batch holds %d entries, want only the decodable app", len(batch))
	}

	// SpecAt: 0 is the newest, a number is that revision, and a corrupt row
	// is an error rather than a zero spec.
	rev, spec, err := SpecAt(st, multi, 0)
	if err != nil || rev.Rev != 2 || spec.Source.Image != "a:2" {
		t.Errorf("SpecAt(multi, 0) = %+v, %+v, %v; want rev 2", rev, spec, err)
	}
	rev, spec, err = SpecAt(st, multi, 1)
	if err != nil || rev.Rev != 1 || spec.Source.Image != "a:1" {
		t.Errorf("SpecAt(multi, 1) = %+v, %+v, %v; want rev 1", rev, spec, err)
	}
	if _, spec, err := SpecAt(st, corrupt, 0); err == nil {
		t.Errorf("SpecAt(corrupt) = %+v, want an error", spec)
	}
	if _, err := ParseSpec(`{not-json`); err == nil {
		t.Error("ParseSpec accepted broken JSON")
	}

	// WithSpecs: every app in ListApps order (by name), the spec nil exactly
	// where LatestSpec errors.
	all, err := WithSpecs(st)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range all {
		names = append(names, a.Name)
		single, serr := LatestSpec(st, a.ID)
		if (serr != nil) != (a.Spec == nil) || (serr == nil && !reflect.DeepEqual(a.Spec, single)) {
			t.Errorf("%s: WithSpecs = %+v, LatestSpec = %+v, %v", a.Name, a.Spec, single, serr)
		}
	}
	if want := []string{"bare", "corrupt", "multi"}; !reflect.DeepEqual(names, want) {
		t.Errorf("WithSpecs order = %v, want %v", names, want)
	}
}
