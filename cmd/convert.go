package cmd

import (
	"fmt"
	"os"

	"xraytool/internal/convert"

	"github.com/spf13/cobra"
)

func convertCmd() *cobra.Command {
	var inputFlag string

	cmd := &cobra.Command{
		Use:   "convert [input]",
		Short: "Convert Xray JSON to share links or vice versa",
		Long:  `Convert Xray JSON configuration to subscription links, or parse subscription links to Xray JSON. Input can be specified as a positional argument, via --input flag, or read from stdin using '-'.`,
		Run: func(cmd *cobra.Command, args []string) {
			var rawInput string

			// Resolve raw input from flag, args, or stdin
			if inputFlag != "" {
				rawInput = inputFlag
			} else if len(args) > 0 {
				rawInput = args[0]
			} else {
				// Check if there is data on stdin
				stat, err := os.Stdin.Stat()
				if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
					rawInput = "-"
				} else {
					_ = cmd.Help()
					return
				}
			}

			input, _, err := convert.ResolveInput(rawInput)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR|failed to resolve input: %v\n", err)
				osExit(1)
			}

			// Normalize and check if input is JSON
			normalizedJSON, isJSON, err := convert.NormalizeJSONInput(input)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR|invalid input format: %v\n", err)
				osExit(1)
			}

			if isJSON {
				// Convert JSON to Share Links
				shareLinks, err := convert.XrayJSONToShareText(normalizedJSON)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR|failed to convert JSON to share links: %v\n", err)
					osExit(1)
				}
				fmt.Print(shareLinks)
			} else {
				// Convert Share Link to Xray JSON
				xrayJSON, err := convert.ShareLinkToXrayJSON(input)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR|failed to convert share links to JSON: %v\n", err)
					osExit(1)
				}
				fmt.Print(xrayJSON)
			}
		},
	}

	cmd.Flags().StringVarP(&inputFlag, "input", "i", "", "Input string, file path, or '-' for stdin")
	return cmd
}
