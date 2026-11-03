package dockerx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A refused GET carries the daemon's own words and status, not just a code.
func TestGetErrorCarriesTheDaemonMessage(t *testing.T) {
	c := engineClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"layer store is corrupt"}`)
	}))
	_, err := c.Inspect(context.Background(), "abc")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if ae.Status != 500 || ae.Message != "layer store is corrupt" || ae.Op != "GET /containers/abc/json" {
		t.Errorf("APIError = %+v", ae)
	}
	if err.Error() != "docker API GET /containers/abc/json: HTTP 500: layer store is corrupt" {
		t.Errorf("err = %q", err)
	}
}

func TestLogsErrorCarriesTheDaemonMessage(t *testing.T) {
	c := engineClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: abc"}`)
	}))
	_, err := c.Logs(context.Background(), "abc", false, "10", false)
	if !hasStatus(err, http.StatusNotFound) || !strings.Contains(err.Error(), "No such container: abc") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "?") {
		t.Errorf("the query string leaked into the error: %v", err)
	}
}

// networkEngine answers the network lookup with lookup, the create with
// create, and counts the creates.
type networkEngine struct {
	lookup, create int
	created        int
}

func (f *networkEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/"+apiVersion)
	switch {
	case r.Method == http.MethodGet && path == "/networks/fog":
		w.WriteHeader(f.lookup)
		if f.lookup == http.StatusOK {
			io.WriteString(w, `{"Name":"fog"}`)
		} else {
			io.WriteString(w, `{"message":"lookup says no"}`)
		}
	case r.Method == http.MethodPost && path == "/networks/create":
		f.created++
		w.WriteHeader(f.create)
		io.WriteString(w, `{"message":"create says no"}`)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func TestEnsureNetwork(t *testing.T) {
	cases := []struct {
		name           string
		lookup, create int
		wantErr        string
		wantCreated    int
	}{
		{"exists", http.StatusOK, 0, "", 0},
		{"missing is created", http.StatusNotFound, http.StatusCreated, "", 1},
		{"created by a racer", http.StatusNotFound, http.StatusConflict, "", 1},
		{"create refused", http.StatusNotFound, http.StatusInternalServerError, "create says no", 1},
		// A daemon that can't answer says nothing about the network: before,
		// any lookup error was read as "missing" and a create was attempted.
		{"lookup fails", http.StatusInternalServerError, http.StatusCreated, "lookup says no", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &networkEngine{lookup: tc.lookup, create: tc.create}
			c := engineClient(t, f)
			err := c.EnsureNetwork(context.Background(), "fog")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want it to say %q", err, tc.wantErr)
			}
			if f.created != tc.wantCreated {
				t.Errorf("creates = %d, want %d", f.created, tc.wantCreated)
			}
		})
	}
}
