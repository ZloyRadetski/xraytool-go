package cmd

import (
	"encoding/json"
	"fmt"

	"xraytool/internal/slave"
	"xraytool/internal/subscription"

	"github.com/spf13/cobra"
)



func applyBatchCmd() *cobra.Command {
	var payloadStr string

	cmd := &cobra.Command{
		Use:   "apply-batch",
		Short: "Apply a batch of user operations at once",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if payloadStr == "" {
				printJSON(map[string]interface{}{"ok": false, "error": "payload is required"})
				return
			}

			var payload slave.BatchPayload
			if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("invalid json payload: %v", err)})
				return
			}

			result := subscription.ApplyBatchOperations(cfg, payload)
			printJSON(result)
		},
	}
	cmd.Flags().StringVar(&payloadStr, "payload", "", "JSON payload containing batch operations")
	return cmd
}
