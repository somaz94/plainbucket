package cli

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/somaz94/plainbucket/internal/config"
)

var healthcheckCmd = &cobra.Command{
	Use:   "healthcheck",
	Short: "Exit 0 when the server on this host answers /healthz",
	Long: "healthcheck requests /healthz from the server configured by the same settings, " +
		"for a container HEALTHCHECK where no shell or curl exists. It needs no access key.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadUnvalidated(cfgFile)
		if err != nil {
			return err
		}
		return healthcheck(healthURL(cfg), 3*time.Second)
	},
}

func init() {
	rootCmd.AddCommand(healthcheckCmd)
}

// healthURL turns the listen address into a URL on this host.
func healthURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		host, port = "", cfg.Address
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	scheme := "http"
	if cfg.HasTLS() {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/healthz"
}

func healthcheck(url string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		// The certificate names the public host, not the address dialed here.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // local probe
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}
