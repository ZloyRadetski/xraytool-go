package cmd

import (
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"os"
	"strings"

	"xraytool/internal/domain"
	"xraytool/internal/plugins/subscription_runtime/runtime"

	"github.com/spf13/cobra"
)

func applyBatchCmd(deps *AppDeps) *cobra.Command {
	var payloadStr string
	var fileStr string

	cmd := &cobra.Command{
		Use:   "apply-batch",
		Short: "Apply a batch of user operations at once",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			var reader io.Reader

			if fileStr == "-" || (len(args) > 0 && args[0] == "-") {
				reader = os.Stdin
			} else if fileStr != "" {
				f, err := os.Open(fileStr)
				if err != nil {
					return fmt.Errorf("failed to open file: %w", err)
				}
				defer f.Close()
				reader = f
			} else if payloadStr != "" {
				reader = strings.NewReader(payloadStr)
			} else if len(args) > 0 {
				reader = strings.NewReader(args[0])
			} else {
				return fmt.Errorf("payload is required via --payload, --file, arg, or stdin ('-')")
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
				return fmt.Errorf("invalid json payload: %w", err)
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
			return nil
		},
	}
	cmd.Flags().StringVar(&payloadStr, "payload", "", "JSON payload containing batch operations")
	cmd.Flags().StringVar(&fileStr, "file", "", "File path to read JSON payload from")
	return cmd
}
