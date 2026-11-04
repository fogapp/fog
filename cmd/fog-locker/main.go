// fog-locker reads a fog offsite locker (docs/locker-contract.md) from any
// machine: list what is in it, pull the newest recovery kit, fetch and
// open any sealed object. It is the insurance that the format outlives
// the daemon — bucket credentials plus the passphrase are enough to get
// everything back without a running fog.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/offsite"
	"golang.org/x/term"
)

// safe renders a bucket-controlled string for the terminal with control
// characters stripped — locker.json's name and the index's fog name are not
// signed, so a bucket writer could otherwise inject ANSI escapes into this
// output.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || (unicode.IsGraphic(r) && !unicode.IsControl(r)) {
			return r
		}
		return '?'
	}, s)
}

func usage() {
	fmt.Fprint(os.Stderr, `fog-locker — read a fog offsite locker without a running fog

usage: fog-locker <command> [flags]

commands:
  lockers       list the lockers under the prefix
  ls            list what the locker holds (kits, apps, Files snapshots)
  restore-kit   download the newest recovery kit (--out, default ./fog-recovery-kit-<stamp>.enc)
  fetch         download and open one sealed object (--object, --out)
  restore-files write a Files snapshot to a directory (--stamp, --out)
  restore-backup write one app backup to a directory (--app, --stamp, --out):
                the artifact.tar (or volume tars and dumps) fog would
                restore from, checked end to end
  analyze       measure what a local backups folder would cost in fog's
                incremental store (--dir; --json for the raw figures).
                Reads only; needs no bucket.

connection flags:
  --endpoint   https://s3.us-west-004.backblazeb2.com
  --region     us-west-004
  --bucket     name
  --prefix     optional path inside the bucket
  --key-id     access key id
  --secret     secret key         (prompted for when omitted)
  --passphrase kit passphrase     (prompted for when omitted)
  --locker     locker id          (default: the only one under the prefix)
  --vhost      use virtual-hosted bucket addressing (default path-style)

Nothing is read from the environment. Prefer the prompts for the secret
and the passphrase: a flag lands in your shell history, a prompt does not.
`)
}

type conn struct {
	endpoint, region, bucket, prefix, keyID, secret, locker, passphrase string
	vhost                                                               bool
}

func (c *conn) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.endpoint, "endpoint", "", "S3 endpoint URL")
	fs.StringVar(&c.region, "region", "", "signing region")
	fs.StringVar(&c.bucket, "bucket", "", "bucket")
	fs.StringVar(&c.prefix, "prefix", "", "prefix inside the bucket")
	fs.StringVar(&c.keyID, "key-id", "", "access key id")
	fs.StringVar(&c.secret, "secret", "", "secret key (prompted for when omitted)")
	fs.StringVar(&c.passphrase, "passphrase", "", "kit passphrase (prompted for when omitted)")
	fs.StringVar(&c.locker, "locker", "", "locker id")
	fs.BoolVar(&c.vhost, "vhost", false, "virtual-hosted bucket addressing")
}

// prompt asks on the controlling terminal with echo off, so a secret typed
// here lands in neither the shell history nor the scrollback. Without a
// terminal (a pipe), the line is read from stdin as it is.
func prompt(label string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprint(os.Stderr, label+": ")
		return readLine(os.Stdin)
	}
	defer tty.Close()
	fmt.Fprint(tty, label+": ")
	fd := int(tty.Fd())
	saved, err := term.GetState(fd)
	if err != nil {
		// Not a terminal after all: read the line as it comes.
		return readLine(tty)
	}
	// A Ctrl-C mid-prompt would exit without running ReadPassword's own
	// restore and leave the shell with echo off: put the terminal back
	// before going.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	defer func() { signal.Stop(sig); close(done) }()
	go func() {
		select {
		case <-sig:
			term.Restore(fd, saved)
			fmt.Fprintln(tty)
			os.Exit(130)
		case <-done:
		}
	}()
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(tty)
	return string(b), err
}

func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// complete fills in what the flags left out — asking, never guessing.
func (c *conn) complete(needPassphrase bool) error {
	for _, f := range []struct {
		name string
		dst  *string
	}{{"endpoint", &c.endpoint}, {"bucket", &c.bucket}, {"key id", &c.keyID}} {
		if *f.dst == "" {
			return fmt.Errorf("--%s is required", strings.ReplaceAll(f.name, " ", "-"))
		}
	}
	var err error
	if c.secret == "" {
		if c.secret, err = prompt("secret key"); err != nil || c.secret == "" {
			return errors.New("a secret key is required")
		}
	}
	if needPassphrase && c.passphrase == "" {
		if c.passphrase, err = prompt("kit passphrase"); err != nil || c.passphrase == "" {
			return errors.New("the kit passphrase is required")
		}
	}
	return nil
}

func (c *conn) client() (*objstore.Client, error) {
	if err := c.complete(false); err != nil {
		return nil, err
	}
	return objstore.New(objstore.Config{Endpoint: c.endpoint, Region: c.region, Bucket: c.bucket,
		AccessKey: c.keyID, SecretKey: c.secret, PathStyle: !c.vhost})
}

// reader resolves the locker (the only one under the prefix unless
// --locker says which) and opens its keys with the passphrase.
func (c *conn) reader(ctx context.Context) (*offsite.Reader, error) {
	if err := c.complete(true); err != nil {
		return nil, err
	}
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	id := c.locker
	if id == "" {
		ids, err := offsite.Discover(ctx, client, c.prefix)
		if err != nil {
			return nil, err
		}
		switch len(ids) {
		case 0:
			return nil, fmt.Errorf("no locker under %s in %s", offsite.LockersRoot(c.prefix), c.bucket)
		case 1:
			id = ids[0]
		default:
			return nil, fmt.Errorf("several lockers under the prefix (%s) — pass --locker", strings.Join(ids, ", "))
		}
	}
	layout := offsite.Layout{Prefix: c.prefix, ID: id}
	lk, err := offsite.FetchLocker(ctx, client, layout)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "opening locker %s (%s) — deriving the key takes a moment\n", id, safe(lk.Name))
	keys, err := lk.OpenWithPassphrase(c.passphrase)
	if err != nil {
		return nil, err
	}
	return &offsite.Reader{Client: client, Layout: layout, Keys: keys}, nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	var c conn
	c.flags(fs)
	var out, object, stamp string
	fs.StringVar(&out, "out", "", "output path")
	fs.StringVar(&object, "object", "", "object name inside the locker (fetch)")
	fs.StringVar(&stamp, "stamp", "", "Drive snapshot stamp (restore-files)")
	var dir, appName string
	var asJSON bool
	fs.StringVar(&appName, "app", "", "app name (restore-backup)")
	fs.StringVar(&dir, "dir", "", "backups folder (analyze)")
	fs.BoolVar(&asJSON, "json", false, "print JSON (analyze)")
	fs.Parse(os.Args[2:])

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	var err error
	switch cmd {
	case "lockers":
		err = cmdLockers(ctx, &c)
	case "ls":
		err = cmdLs(ctx, &c)
	case "restore-kit":
		err = cmdRestoreKit(ctx, &c, out)
	case "fetch":
		err = cmdFetch(ctx, &c, object, out)
	case "restore-files":
		err = cmdRestoreFiles(ctx, &c, stamp, out)
	case "restore-backup":
		err = cmdRestoreBackup(ctx, &c, appName, stamp, out)
	case "analyze":
		err = cmdAnalyze(ctx, dir, asJSON)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fog-locker:", err)
		os.Exit(1)
	}
}

func cmdLockers(ctx context.Context, c *conn) error {
	client, err := c.client()
	if err != nil {
		return err
	}
	ids, err := offsite.Discover(ctx, client, c.prefix)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("no lockers under", offsite.LockersRoot(c.prefix))
		return nil
	}
	for _, id := range ids {
		lk, err := offsite.FetchLocker(ctx, client, offsite.Layout{Prefix: c.prefix, ID: id})
		if err != nil {
			fmt.Printf("%s  (unreadable: %v)\n", id, err)
			continue
		}
		fmt.Printf("%s  %s  created %s\n", id, safe(lk.Name), safe(lk.CreatedAt))
	}
	return nil
}

func cmdLs(ctx context.Context, c *conn) error {
	r, err := c.reader(ctx)
	if err != nil {
		return err
	}
	kits, err := r.Kits(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("locker %s\n\nrecovery kits (%d):\n", r.Layout.ID, len(kits))
	for i, k := range kits {
		if i >= 5 {
			fmt.Printf("  … and %d older\n", len(kits)-5)
			break
		}
		fmt.Printf("  %s  %s\n", k.Name, fsx.HumanBytes(k.Bytes))
	}
	ix, err := r.Index(ctx)
	switch {
	case errors.Is(err, offsite.ErrNoIndex):
		fmt.Println("\nno index yet — listing")
		apps, err := r.Apps(ctx)
		if err != nil {
			return err
		}
		for _, app := range apps {
			rows, err := r.AppRows(ctx, app)
			if err != nil {
				return err
			}
			printApp(app, rows)
		}
		stamps, err := r.FilesSnapshots(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("\nFiles snapshots (%d):\n", len(stamps))
		for i, s := range stamps {
			if i >= 5 {
				fmt.Printf("  … and %d older\n", len(stamps)-5)
				break
			}
			fmt.Printf("  %s\n", s)
		}
	case err != nil:
		return err
	default:
		fmt.Printf("\nindex written %s by %q\n", safe(ix.WrittenAt), safe(ix.Fog))
		apps := make([]string, 0, len(ix.Apps))
		for a := range ix.Apps {
			apps = append(apps, a)
		}
		sort.Strings(apps)
		for _, app := range apps {
			printApp(app, ix.Apps[app])
		}
		fmt.Printf("\nFiles snapshots (%d):\n", len(ix.Files))
		for i, f := range ix.Files {
			if i >= 5 {
				fmt.Printf("  … and %d older\n", len(ix.Files)-5)
				break
			}
			fmt.Printf("  %s  %d files  %s\n", f.Stamp, f.Files, fsx.HumanBytes(f.Bytes))
		}
	}
	return nil
}

func printApp(app string, rows []offsite.AppRow) {
	fmt.Printf("\n%s (%d backups):\n", safe(app), len(rows))
	for i, row := range rows {
		if i >= 5 {
			fmt.Printf("  … and %d older\n", len(rows)-5)
			break
		}
		verified := ""
		if row.VerifiedAt != "" {
			verified = "  verified"
		}
		fmt.Printf("  %s  %-10s %-9s %s%s\n", row.Stamp, row.Kind, row.Mode, fsx.HumanBytes(row.SizeBytes), verified)
	}
}

// safeOutName reduces a kit name read from the bucket to a bare filename to
// write in the working directory. The bucket is untrusted: a name like
// "../../etc/cron.d/x" must not steer the write out of the current directory.
func safeOutName(name string) (string, error) {
	// A well-formed kit name is a bare "fog-recovery-kit-<stamp>.enc" with no
	// path components. Anything with a slash, a traversal, or a NUL is refused
	// rather than quietly reshaped, so a doctored bucket cannot pick where the
	// file lands — the operator passes --out instead.
	if name == "" || name == "." || name == ".." ||
		name != filepath.Base(name) ||
		strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) ||
		strings.Contains(name, "\x00") {
		return "", fmt.Errorf("the kit's name in the bucket (%q) is not a safe filename — pass --out", name)
	}
	return name, nil
}

func cmdRestoreKit(ctx context.Context, c *conn, out string) error {
	r, err := c.reader(ctx)
	if err != nil {
		return err
	}
	// The same choice fog's own cloud recovery makes (offsite.OpenNewestKit):
	// kits fog recorded uploading first, current-format kits before legacy
	// ones, future-dated names ignored. Kits passed over are said so, and so
	// is a choice that is not the newest.
	kit, err := offsite.OpenNewestKit(ctx, r, c.passphrase)
	if err != nil {
		return err
	}
	for _, sk := range kit.Skipped {
		fmt.Fprintf(os.Stderr, "WARNING: skipped %s: %s\n", safe(sk.Name), safe(sk.Reason))
	}
	if kit.FellBack {
		latest := kit.Latest
		if latest == "" || latest == kit.Name {
			latest = "a newer kit in the bucket"
		}
		fmt.Fprintf(os.Stderr, "WARNING: this is NOT the newest kit — %s could not be used. Restoring %s rolls the machine back to an older state.\n", safe(latest), safe(kit.Name))
	}
	if !kit.Recorded {
		fmt.Fprintf(os.Stderr, "note: %s is not in the locker's sealed index of kits fog uploaded (no index, or an older fog)\n", safe(kit.Name))
	}
	fmt.Fprintf(os.Stderr, "kit sha256 %s\n", kit.SHA256)
	if kit.Check.Legacy {
		date := kit.Check.Date
		if date == "" {
			date = "unknown"
		}
		fmt.Fprintf(os.Stderr, "WARNING: %s is a legacy kit (inside it: %s): %s\n", kit.Name, safe(date), backup.LegacyKitWarning)
	} else if kit.Check.Date != "" {
		fmt.Fprintf(os.Stderr, "kit sealed %s\n", safe(kit.Check.Date))
	}
	if out == "" {
		// The default name comes from the bucket listing — an attacker with
		// write access to the bucket can name an object "../../etc/evil", so
		// it is reduced to a bare filename before it becomes a path to write.
		safe, err := safeOutName(kit.Name)
		if err != nil {
			return err
		}
		out = safe
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := f.Write(kit.Sealed)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
		return err
	}
	fmt.Printf("wrote %s (%s) — upload it on a fresh fog's setup screen with the same passphrase\n", out, fsx.HumanBytes(int64(n)))
	return nil
}

func cmdFetch(ctx context.Context, c *conn, object, out string) error {
	if object == "" {
		return errors.New("--object is required (a name from `ls`, e.g. apps/hub/20260827-010314.snapshot)")
	}
	r, err := c.reader(ctx)
	if err != nil {
		return err
	}
	key := r.Layout.Root() + strings.TrimPrefix(object, "/")
	if out == "" || out == "-" {
		rc, err := r.Open(ctx, key)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(os.Stdout, rc)
		return err
	}
	sum, n, err := r.FetchTo(ctx, key, out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s) sha256 %s\n", out, fsx.HumanBytes(n), sum)
	return nil
}

func cmdRestoreFiles(ctx context.Context, c *conn, stamp, out string) error {
	if out == "" {
		return errors.New("--out is required: the directory to write the snapshot into")
	}
	r, err := c.reader(ctx)
	if err != nil {
		return err
	}
	if stamp == "" {
		stamps, err := r.FilesSnapshots(ctx)
		if err != nil {
			return err
		}
		if len(stamps) == 0 {
			return errors.New("the locker holds no Drive snapshot")
		}
		stamp = stamps[0]
	}
	n, bytes, err := r.RestoreFiles(ctx, stamp, out, func(path string) { fmt.Println(" ", path) })
	if err != nil {
		return err
	}
	fmt.Printf("restored %d files (%s) from snapshot %s into %s\n", n, fsx.HumanBytes(bytes), stamp, out)
	return nil
}
