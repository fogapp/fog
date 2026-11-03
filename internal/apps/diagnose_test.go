package apps

import "testing"

func TestDiagnose(t *testing.T) {
	cases := []struct {
		log       string
		wantTitle string
	}{
		{"git@gitea.lan: Permission denied (publickey,password).\r\nfatal: Could not read from remote repository.", "The git server rejected the connection"},
		{"Permission denied (publickey).", "The git server rejected the connection"},
		{"ssh: Could not resolve hostname gitea.lna: Name or service not known", "The repository's server can't be found"},
		{"ERROR: repository not found", "No repository at that URL"},
		{"branch develop not found on the remote", "The branch doesn't exist"},
		{"no Dockerfile at Dockerfile in the repository", "No Dockerfile in the repository"},
		{"#12 ERROR: failed to solve: process \"/bin/sh -c bundle install\" did not complete", "The image build failed"},
		{"rails aborted!\nrelease command failed", "The release step (usually migrations) failed"},
		{"new replica never became healthy — old version kept running", "The app started but never answered its health check"},
		{"write /var/lib/docker: no space left on device", "The server is out of disk space"},
		{"NAS share media (/mnt/nas/media) is needed by this app but wouldn't mount: mount failed: timed out", "A NAS share this app needs couldn't be mounted"},
		{"secret RAILS_MASTER_KEY: not found", "A required secret has no value"},
	}
	for _, c := range cases {
		d := Diagnose(c.log)
		if d == nil {
			t.Errorf("no diagnosis for %q", c.log)
			continue
		}
		if d.Title != c.wantTitle {
			t.Errorf("log %q → %q, want %q", c.log, d.Title, c.wantTitle)
		}
		if d.Fix == "" {
			t.Errorf("%q has no fix text", d.Title)
		}
	}
	if Diagnose("something completely novel went wrong") != nil {
		t.Error("unknown failures must return nil so the raw log is shown")
	}
}

func TestLogTail(t *testing.T) {
	if got := LogTail("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
	long := "line1\nline2\nline3"
	got := LogTail(long, 11)
	if got != "line2\nline3" {
		t.Errorf("got %q", got)
	}
}
