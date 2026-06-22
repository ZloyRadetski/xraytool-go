package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	
	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/subscription"

	"github.com/spf13/cobra"
)

func subCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sub",
		Short: "Generate subscription payload for a client from JSON request on stdin",
		Run: func(cmd *cobra.Command, _ []string) {
			// Read from Stdin
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				resp := failResponse(500, fmt.Sprintf("failed to read stdin: %v", err))
				printResponse(resp)
				return
			}

			var req subscription.Request
			if err := json.Unmarshal(data, &req); err != nil {
				resp := failResponse(400, fmt.Sprintf("invalid request JSON: %v", err))
				printResponse(resp)
				return
			}

			cm := subscription.NewCacheManager(cfg)
			cm.Refresh() // Single refresh for CLI usage
			
			// Init DB since we need it for ProcessSQL
			if err := database.Init(database.Config{
				Driver:     cfg.Database.Driver,
				DSN:        cfg.Database.DSN,
				SQLitePath: cfg.Database.SQLitePath,
			}); err != nil {
				printResponse(failResponse(500, "database error"))
				return
			}
			defer database.Close()
			
			dispatcher := events.NewDispatcher(cfg)
			resp := subscription.ProcessSQL(database.DB(), cm, dispatcher, &req)
			printResponse(resp)
		},
	}
	return cmd
}

func failResponse(code int, msg string) *subscription.Response {
	return &subscription.Response{
		StatusCode: code,
		Headers: map[string]string{
			"Content-Type": "text/plain; charset=utf-8",
		},
		Body: msg,
	}
}

func printResponse(resp *subscription.Response) {
	out, err := json.Marshal(resp)
	if err != nil {
		// Safe fallback
		fmt.Printf(`{"status_code":500,"headers":{"Content-Type":"text/plain"},"body":"internal json marshal error"}`)
		return
	}
	fmt.Println(string(out))
}
