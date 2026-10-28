package cli

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/storage"
)

var fsckRepair, fsckVerify bool

var fsckCmd = &cobra.Command{
	Use:   "fsck",
	Short: "Check the root for stale metadata and leftovers while the server is stopped",
	Long: "fsck reports objects whose metadata is missing or outdated, files whose names cannot be " +
		"keys, and leftovers such as staging files and expired multipart uploads. With --repair it " +
		"rebuilds the metadata and removes the leftovers. With --verify it also reads every object and " +
		"compares its data with the ETag and checksums recorded when it was written, which finds damage " +
		"that keeps a file's size and modification time. It refuses to run while a server uses the root.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadUnvalidated(cfgFile)
		if err != nil {
			return err
		}
		store, err := storage.New(cfg.Root)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		switch err := store.Lock(); {
		case errors.Is(err, storage.ErrLocked):
			return fmt.Errorf("%s: %w; stop the server first", cfg.Root, err)
		case errors.Is(err, errors.ErrUnsupported):
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: cannot lock the root here; make sure no server uses it")
		case err != nil:
			return fmt.Errorf("locking %s: %w", cfg.Root, err)
		}
		store.OnSyncError(syncWarnings(cmd.ErrOrStderr()))
		defer func() {
			if n := store.SyncErrors(); n > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d directory flush(es) failed; what fsck wrote is in place but may not survive a power loss\n", n)
			}
		}()
		out := cmd.OutOrStdout()
		var found, unfixed int
		err = store.Fsck(cmd.Context(), cfg.UploadExpiry, fsckRepair, func(f storage.Finding) {
			found++
			state := ""
			switch {
			case f.Fixed:
				state = " (fixed)"
			case fsckRepair:
				state = " (not fixed)"
				unfixed++
			default:
				unfixed++
			}
			_, _ = fmt.Fprintf(out, "%s: %s%s\n", f.Kind, f.Path, state)
		})
		if err != nil {
			return err
		}
		if fsckVerify {
			res, err := store.Scrub(cmd.Context(), 0, func(c storage.Corruption) {
				found++
				unfixed++ // nothing to rebuild it from
				if c.What == "read" {
					_, _ = fmt.Fprintf(out, "corrupt object: %s/%s (unreadable: %s)\n", c.Bucket, c.Key, c.Got)
				} else {
					_, _ = fmt.Fprintf(out, "corrupt object: %s/%s (%s recorded %s, data gives %s)\n", c.Bucket, c.Key, c.What, c.Want, c.Got)
				}
			})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "verified %d object(s), %d bytes; %d skipped (no trusted metadata, or changing), %d with nothing recorded to check\n",
				res.Objects, res.Bytes, res.Skipped, res.Unchecked)
		}
		_, _ = fmt.Fprintf(out, "%d problem(s) found, %d left\n", found, unfixed)
		if unfixed > 0 {
			return errors.New("fsck found problems")
		}
		return nil
	},
}

// syncWarnings prints the first failed directory flush and an unsupported one; the
// count follows at the end.
func syncWarnings(w io.Writer) func(dir string, err error) {
	var once sync.Once
	return func(dir string, err error) {
		if errors.Is(err, storage.ErrSyncUnsupported) {
			_, _ = fmt.Fprintf(w, "warning: %s: %v; what fsck writes may not survive a power loss\n", dir, err)
			return
		}
		once.Do(func() { _, _ = fmt.Fprintf(w, "warning: flushing %s: %v\n", dir, err) })
	}
}

func init() {
	fsckCmd.Flags().BoolVar(&fsckRepair, "repair", false, "rebuild metadata and remove leftovers")
	fsckCmd.Flags().BoolVar(&fsckVerify, "verify", false, "read every object and check its data against its recorded digests")
	rootCmd.AddCommand(fsckCmd)
}
