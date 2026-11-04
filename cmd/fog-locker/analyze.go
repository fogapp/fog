package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/chunkstore"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/offsite"
)

// cmdAnalyze measures a backups folder against the incremental store
// without changing it: the figures that decide chunk size, what the store
// saves, and whether databases dominate what changes each night.
func cmdAnalyze(ctx context.Context, dir string, asJSON bool) error {
	if dir == "" {
		return errors.New("--dir is required: the backups folder (Settings → Storage → Drive shows where it is)")
	}
	rep, err := chunkstore.Analyze(ctx, dir, backup.IsReservedDir, chunkstore.AnalyzeOptions{
		Progress: func(series, stamp string) { fmt.Fprintf(os.Stderr, "reading %s %s\n", series, stamp) },
	})
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	h := fsx.HumanBytes
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "series\tsnapshots\ton disk today\tstore, as is\tnightly\tstore, uncompressed\tnightly\tdb share\t")
	row := func(s chunkstore.SeriesReport) {
		share := "—"
		if n := s.Expanded.Stored; n > 0 {
			share = fmt.Sprintf("%.0f%%", 100*float64(s.Expanded.DBStored)/float64(n))
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", s.Series, s.Snapshots, h(s.OnDisk),
			h(s.AsIs.Stored), h(s.AsIs.Median()), h(s.Expanded.Stored), h(s.Expanded.Median()), share)
	}
	for _, s := range rep.Series {
		row(s)
	}
	row(rep.Totals())
	w.Flush()
	fmt.Println()
	fmt.Println("store, as is:          today's artifacts chunked and deduplicated as they are")
	fmt.Println("store, uncompressed:   with the zstd/gzip archives inside opened — what fog-backup/2 producers will cost")
	fmt.Println("nightly:               median bytes a night adds once the history exists")
	fmt.Println("db share:              pg_dump custom-format dumps are compressed inside; a -Z0 dump dedupes better than shown.")
	fmt.Println("                       A share that stays high after fog-backup/2 is the signal for continuous database protection.")
	return nil
}

// cmdRestoreBackup writes one app backup out of the locker.
func cmdRestoreBackup(ctx context.Context, c *conn, app, stamp, out string) error {
	if app == "" || stamp == "" {
		return errors.New("--app and --stamp are required (ls lists them)")
	}
	if out == "" {
		out = app + "-" + stamp
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s already exists", out)
	}
	r, err := c.reader(ctx)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(filepath.Dir(filepath.Clean(out)), ".fog-locker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	fmt.Fprintf(os.Stderr, "fetching %s %s\n", app, stamp)
	row, err := offsite.FetchBackup(ctx, r, app, stamp, scratch, out)
	if err != nil {
		os.RemoveAll(out)
		return err
	}
	fmt.Printf("wrote %s (%s backup, %s)\n", out, row.Mode, row.CreatedAt)
	return nil
}
