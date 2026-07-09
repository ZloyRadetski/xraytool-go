package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"xraytool/internal/domain"
	"xraytool/internal/subscription"

	"github.com/spf13/cobra"
)

func applyBatchCmd(deps *AppDeps) *cobra.Command {
	var payloadStr string
	var fileStr string

	cmd := &cobra.Command{
		Use:   "apply-batch",
		Short: "Apply a batch of user operations at once",
		Run: func(cmd *cobra.Command, args []string) {
			requireRoot() //nolint:errcheck

			var reader io.Reader

			if fileStr == "-" || (len(args) > 0 && args[0] == "-") {
				reader = os.Stdin
			} else if fileStr != "" {
				f, err := os.Open(fileStr)
				if err != nil {
					printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("failed to open file: %v", err)})
					return
				}
				defer f.Close()
				reader = f
			} else if payloadStr != "" {
				reader = strings.NewReader(payloadStr)
			} else if len(args) > 0 {
				reader = strings.NewReader(args[0])
			} else {
				printJSON(map[string]interface{}{"ok": false, "error": "payload is required via --payload, --file, arg, or stdin ('-')"})
				return
			}

			var payloadDTO struct {
				Add []struct {
					Email   string   `json:"email"`
					UUID    string   `json:"uuid,omitempty"`
					Auth    string   `json:"auth,omitempty"`
					Subfile string   `json:"subfile"`
					Expire  string   `json:"expire"`
					Limit   *float64 `json:"limit,omitempty"`
				} `json:"add"`
				Remove []string `json:"remove"`
			}

			if err := json.NewDecoder(reader).Decode(&payloadDTO); err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": fmt.Sprintf("invalid json payload: %v", err)})
				return
			}

			var payload domain.BatchPayload
			payload.Remove = payloadDTO.Remove
			for _, u := range payloadDTO.Add {
				cfg := domain.VPNUserConfig{
					Email:   u.Email,
					UUID:    u.UUID,
					Auth:    u.Auth,
					Subfile: u.Subfile,
					Expire:  u.Expire,
				}
				if u.Limit != nil {
					cfg.MaxDevices = int(*u.Limit)
				}
				payload.Add = append(payload.Add, cfg)
			}

			result := subscription.ApplyBatchOperations(deps.Engine, payload)
			printJSON(result)
		},
	}
	cmd.Flags().StringVar(&payloadStr, "payload", "", "JSON payload containing batch operations")
	cmd.Flags().StringVar(&fileStr, "file", "", "File path to read JSON payload from")
	return cmd
}
