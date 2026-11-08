package appevents

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/store/storetest"
)

func testBus(t *testing.T) *Bus {
	t.Helper()
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st)
}

func TestPublishForcesSourceAndDedupes(t *testing.T) {
	b := testBus(t)
	ev := Event{ID: "nzo_1", Source: "spoofed", Type: "sabnzbd.download.completed", Data: json.RawMessage(`{"name":"x"}`)}
	if n, err := b.Publish("glass", []Event{ev}); err != nil || n != 1 {
		t.Fatalf("first publish = %d, %v", n, err)
	}
	// A retry of the same id is absorbed; the same id from another app is not.
	if n, _ := b.Publish("glass", []Event{ev}); n != 0 {
		t.Fatalf("retry inserted %d", n)
	}
	if n, _ := b.Publish("hook", []Event{ev}); n != 1 {
		t.Fatalf("other source inserted %d", n)
	}
	p, err := b.Read(context.Background(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 2 || p.Events[0].Source != "glass" || p.Events[1].Source != "hook" {
		t.Fatalf("events = %+v", p.Events)
	}
	if p.Events[0].SpecVersion != "1.0" || p.Events[0].DataContentType != "application/json" || p.Cursor != p.Events[1].Seq {
		t.Fatalf("page = %+v", p)
	}
}

func TestPublishRejects(t *testing.T) {
	b := testBus(t)
	for name, ev := range map[string]Event{
		"no type":   {},
		"bad type":  {Type: "Has Spaces"},
		"bad time":  {Type: "a.b", Time: "yesterday"},
		"bad data":  {Type: "a.b", Data: json.RawMessage(`{`)},
		"long id":   {Type: "a.b", ID: string(make([]byte, 200))},
		"huge data": {Type: "a.b", Data: json.RawMessage(`"` + string(make([]byte, MaxDataSize)) + `"`)},
	} {
		if _, err := b.Publish("app", []Event{ev}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestReadFromNowAndLongPoll(t *testing.T) {
	b := testBus(t)
	b.PublishFog("fog.notice", "", map[string]any{"title": "old"})
	start, err := b.Read(context.Background(), -1, 0, 0)
	if err != nil || len(start.Events) != 0 || start.Cursor == 0 {
		t.Fatalf("from now = %+v, %v", start, err)
	}
	got := make(chan Page, 1)
	go func() {
		p, _ := b.Read(context.Background(), start.Cursor, 0, 5*time.Second)
		got <- p
	}()
	time.Sleep(50 * time.Millisecond)
	b.PublishFog("fog.notice", "", map[string]any{"title": "new"})
	select {
	case p := <-got:
		if len(p.Events) != 1 || p.Events[0].Source != FogSource {
			t.Fatalf("woke with %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll never woke")
	}
}

func TestReadReportsGap(t *testing.T) {
	b := testBus(t)
	for i := 0; i < 3; i++ {
		b.PublishFog("fog.notice", "", nil)
	}
	if _, err := b.Store.PruneAppEvents(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	b.PublishFog("fog.notice", "", nil)
	p, _ := b.Read(context.Background(), 1, 0, 0)
	if !p.Gap || len(p.Events) != 1 {
		t.Fatalf("page = %+v, want a gap and one event", p)
	}
}
