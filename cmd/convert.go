package cmd

import (
	"fmt"
	"strings"

	"xraytool/internal/plugins/subscription_format_legacy/convert"

	"github.com/spf13/cobra"
)

func convertCmd(deps *AppDeps) *cobra.Command {
	var inputFlag string
	var formatFlag string

	cmd := &cobra.Command{
		Use:   "convert [input]",
		Short: "Convert Xray JSON to share links, Clash YAML, or vice versa",
		Long:  `Convert Xray JSON configuration to subscription links (--format vless) or a Clash/Mihomo YAML subscription (--format clash), or parse subscription links to Xray JSON. Input can be specified as a positional argument, via --input flag, or read from stdin using '-'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var rawInput string

			// Resolve raw input from flag, args, or stdin
			if inputFlag != "" {
				rawInput = inputFlag
			} else if len(args) > 0 {
				rawInput = args[0]
			} else {
				_ = cmd.Help()
				return nil
			}

			input, _, err := convert.ResolveInput(rawInput)
			if err != nil {
				return fmt.Errorf("failed to resolve input: %v", err)
			}

			// Normalize and check if input is JSON
			normalizedJSON, isJSON, err := convert.NormalizeJSONInput(input)
			if err != nil {
				return fmt.Errorf("invalid input format: %v", err)
			}

			format := strings.ToLower(strings.TrimSpace(formatFlag))
			switch format {
			case "", "vless", "clash":
			default:
				return fmt.Errorf("unsupported format: %s (supported: vless, clash)", formatFlag)
			}

			if isJSON {
				if format == "clash" {
					clashYAML, err := convert.XrayJSONToClashYAML(normalizedJSON)
					if err != nil {
						return fmt.Errorf("failed to convert JSON to clash config: %v", err)
					}
					fmt.Print(clashYAML)
					return nil
				}

				// Convert JSON to Share Links
				shareLinks, err := convert.XrayJSONToShareText(normalizedJSON)
				if err != nil {
					return fmt.Errorf("failed to convert JSON to share links: %v", err)
				}
				fmt.Print(shareLinks)
			} else {
				if format == "clash" {
					clashYAML, err := convert.ShareTextToClashYAML(input)
					if err != nil {
						return fmt.Errorf("failed to convert share links to clash config: %v", err)
					}
					fmt.Print(clashYAML)
					return nil
				}

				// Convert Share Link to Xray JSON
				xrayJSON, err := convert.ShareLinkToXrayJSON(input)
				if err != nil {
					return fmt.Errorf("failed to convert share links to JSON: %v", err)
				}
				fmt.Print(xrayJSON)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&inputFlag, "input", "i", "", "Input string, file path, or '-' for stdin")
	cmd.Flags().StringVarP(&formatFlag, "format", "f", "", "Output format: vless (default share links) or clash")
	return cmd
}
