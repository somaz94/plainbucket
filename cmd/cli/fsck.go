package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/storage"
)

var fsckRepair bool

var fsckCmd = &cobra.Command{
	Use:   "fsck",
	Short: "Check the root for stale metadata and leftovers while the server is stopped",
	Long: "fsck reports objects whose metadata is missing or outdated, files whose names cannot be " +
		"keys, and leftovers such as staging files and expired multipart uploads. With --repair it " +
		"rebuilds the metadata and removes the leftovers. It refuses to run while a server uses the root.",
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
		_, _ = fmt.Fprintf(out, "%d problem(s) found, %d left\n", found, unfixed)
		if unfixed > 0 {
			return errors.New("fsck found problems")
		}
		return nil
	},
}

func init() {
	fsckCmd.Flags().BoolVar(&fsckRepair, "repair", false, "rebuild metadata and remove leftovers")
	rootCmd.AddCommand(fsckCmd)
}
