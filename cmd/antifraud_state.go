package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"xraytool/internal/appconfig"
)

func init() {
	rootCmd.AddCommand(antiFraudStateCmd)
}

var antiFraudStateCmd = &cobra.Command{
	Use:   "ips",
	Short: "Show current connected users and their active IP count (AntiFraud)",
	Long:  "Fetches the live snapshot of tracked IPs from the running Anti-Fraud module.",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := appconfig.Load(cfgFile)
		if err != nil {
			fmt.Printf("ERROR|failed to load config: %v\n", err)
			os.Exit(1)
		}

		if !cfg.AntiFraud.Enabled {
			fmt.Println("Anti-Fraud is DISABLED in config.")
			os.Exit(0)
		}

		apiKey := cfg.Server.APIKey
		if apiKey == "" {
			fmt.Printf("ERROR|server.api_key not found in xraytool.yml\n")
			os.Exit(1)
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/admin/antifraud/state", cfg.Ports.APIServer)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			fmt.Printf("ERROR|creating request: %v\n", err)
			os.Exit(1)
		}

		req.Header.Set("X-API-Key", apiKey)

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("ERROR|API request failed (is the server running?): %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("ERROR|API returned status: %d\n", resp.StatusCode)
			os.Exit(1)
		}

		var result struct {
			Enabled      bool           `json:"enabled"`
			State        map[string]int `json:"state"`
			ActiveSlaves int            `json:"active_slaves"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			fmt.Printf("ERROR|failed to decode response: %v\n", err)
			os.Exit(1)
		}

		if !result.Enabled {
			fmt.Println("Anti-Fraud is disabled on the server.")
			return
		}

		fmt.Printf("Active Slaves reporting stats: %d\n", result.ActiveSlaves)
		
		if len(result.State) == 0 {
			fmt.Println("No active IPs currently tracked.")
			return
		}

		fmt.Printf("Active Users (Tracking IPs over %s):\n", cfg.AntiFraud.IPLimitTTL)
		fmt.Println("---------------------------------------------------------")
		for email, count := range result.State {
			fmt.Printf("%-35s : %d IP(s)\n", email, count)
		}
		fmt.Println("---------------------------------------------------------")
		fmt.Printf("Total tracked users: %d\n", len(result.State))
	},
}
