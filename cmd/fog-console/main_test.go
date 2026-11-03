package main

import (
	"strings"
	"testing"
)

// The banner is printed by agetty, which reads backslashes as escape
// sequences (\4 becomes an IP, \l a tty name). Nothing here may contain one.
func TestRenderHasNoAgettyEscapes(t *testing.T) {
	if got := render(true); strings.Contains(got, `\`) {
		t.Errorf("issue banner contains a backslash agetty would expand:\n%s", got)
	}
}

func TestRenderPointsAtTheBrowser(t *testing.T) {
	got := render(true)
	if !strings.Contains(got, ":8080") {
		t.Errorf("banner never names the dashboard port:\n%s", got)
	}
	// The login prompt below the banner is real, so the banner says whose.
	if !strings.Contains(got, "Log in as") && !strings.Contains(got, "No console account") {
		t.Errorf("banner says nothing about the login prompt beneath it:\n%s", got)
	}
}

// The login greeting sits above a shell that already exists; repeating
// "log in" there would be nonsense.
func TestRenderForLoginOmitsTheLoginLine(t *testing.T) {
	if got := render(false); strings.Contains(got, "Log in as") {
		t.Errorf("greeting tells an already-logged-in operator to log in:\n%s", got)
	}
}

func TestParseConsoleAccounts(t *testing.T) {
	passwd := strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin",
		"kiosk:x:1001:1001::/home/kiosk:/usr/sbin/nologin",
		"matthew:x:1000:1000::/home/matthew:/bin/bash",
		"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin",
		"truncated:line",
		"",
	}, "\n")
	got := parseConsoleAccounts(strings.NewReader(passwd))
	if len(got) != 1 || got[0] != "matthew" {
		t.Errorf("console accounts = %v, want [matthew]", got)
	}
}
