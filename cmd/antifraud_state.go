package cmd

import (
	"fmt"
	json "github.com/goccy/go-json"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

func antiFraudStateCmd(deps *AppDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ips",
		Short: "Show current connected users and their active IP count (AntiFraud)",
		Long:  "Fetches the live snapshot of tracked IPs from the running Anti-Fraud module.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if deps.Cfg == nil {
				return fmt.Errorf("failed to load config")
			}

			if !deps.Cfg.AntiFraud.Enabled {
				fmt.Println("Anti-Fraud is DISABLED in config.")
				return nil
			}

			apiKey := deps.Cfg.Server.APIKey
			if apiKey == "" {
				return fmt.Errorf("server.api_key not found in xraytool.yml")
			}

			url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/admin/antifraud/state", deps.Cfg.Ports.APIServer)
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return fmt.Errorf("creating request: %v", err)
			}

			req.Header.Set("X-API-Key", apiKey)

			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("API request failed (is the server running?): %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("API returned status: %d", resp.StatusCode)
			}

			var result struct {
				Enabled      bool                `json:"enabled"`
				State        map[string][]string `json:"state"`
				ActiveSlaves int                 `json:"active_slaves"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return fmt.Errorf("failed to decode response: %v", err)
			}

			if !result.Enabled {
				fmt.Println("Anti-Fraud is currently DISABLED on the server.")
				return nil
			}

			fmt.Printf("Active Slaves reporting stats: %d\n\n", result.ActiveSlaves)
			if len(result.State) == 0 {
				fmt.Println("No active users tracked by AntiFraud in the current time window.")
				return nil
			}

			fmt.Println("Current Active IP Counts:")
			for email, hashes := range result.State {
				fmt.Printf("  %s: %d IPs (Hashes: %v)\n", email, len(hashes), hashes)
			}
			fmt.Println("---------------------------------------------------------")
			for email, hashes := range result.State {
				fmt.Printf("%-35s : %d IP(s)\n", email, len(hashes))
			}
			fmt.Println("---------------------------------------------------------")
			fmt.Printf("Total tracked users: %d\n", len(result.State))
			return nil
		},
	}
	return cmd
}
