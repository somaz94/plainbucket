package cli

import (
	"bufio"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

var inventoryBucket, inventoryPrefix, inventoryFormat string

var inventoryHeader = []string{"bucket", "key", "size", "last_modified", "etag", "checksum", "content_type", "metadata"}

var inventoryCmd = &cobra.Command{
	Use:   "inventory",
	Short: "List every object with its size, ETag and checksums as CSV or JSON Lines",
	Long: "inventory reads the root and writes one record per object, in S3 order, as CSV with a " +
		"header row or as JSON Lines: bucket, key (URL-encoded, as encoding-type=url lists it), size, " +
		"last_modified, etag (without quotes), checksum, content_type and metadata. metadata says where " +
		"etag, checksum and content_type come from: the server's record (recorded), the file alone " +
		"because its record no longer matches (edited) or it has none (missing), or a directory listed " +
		"as \"<dir>/\" (folder). It only reads and creates nothing, so it can run beside a server or on " +
		"a read-only copy of the root.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if inventoryFormat != "csv" && inventoryFormat != "jsonl" {
			return fmt.Errorf("--format must be csv or jsonl, not %q", inventoryFormat)
		}
		cfg, err := config.LoadUnvalidated(cfgFile)
		if err != nil {
			return err
		}
		store, err := storage.OpenReadOnly(cfg.Root)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		bw := bufio.NewWriter(cmd.OutOrStdout())
		enc := json.NewEncoder(bw)
		enc.SetEscapeHTML(false)
		write := func(e storage.InventoryEntry) error { return enc.Encode(inventoryRecord(e)) }
		var w *csv.Writer
		if inventoryFormat == "csv" {
			w = csv.NewWriter(bw)
			if err := w.Write(inventoryHeader); err != nil {
				return err
			}
			write = func(e storage.InventoryEntry) error {
				e = inventoryRecord(e)
				return w.Write([]string{e.Bucket, e.Key, strconv.FormatInt(e.Size, 10), e.ModTime.Format(time.RFC3339Nano),
					e.ETag, e.Checksum, e.ContentType, e.Metadata})
			}
		}
		err = store.Inventory(cmd.Context(), inventoryBucket, inventoryPrefix, write)
		if w != nil {
			w.Flush()
			err = cmp.Or(err, w.Error())
		}
		return cmp.Or(err, bw.Flush())
	},
}

// inventoryRecord shapes e for output: a URL-encoded key, so neither a spreadsheet formula,
// a line break nor bytes that are not UTF-8 survive into the file, and a bare ETag.
func inventoryRecord(e storage.InventoryEntry) storage.InventoryEntry {
	e.Key, e.ETag = sigv4.URIEncodePath(e.Key), strings.Trim(e.ETag, `"`)
	return e
}

func init() {
	inventoryCmd.Flags().StringVar(&inventoryBucket, "bucket", "", "only this bucket")
	inventoryCmd.Flags().StringVar(&inventoryPrefix, "prefix", "", "only keys starting with this")
	inventoryCmd.Flags().StringVar(&inventoryFormat, "format", "csv", "csv or jsonl")
	rootCmd.AddCommand(inventoryCmd)
}
