package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"xraytool/internal/pluginapi"
	autoBalancerPlugin "xraytool/internal/plugins/subscription_autobalancer"
	"xraytool/internal/plugins/subscription_format_legacy/convert"
)

// subscriptionCmd exposes local validation and rendering for the versioned
// JSON subscription source format. It deliberately does not modify the input
// file, so a template can be checked safely before it is deployed.
func subscriptionCmd(_ *AppDeps) *cobra.Command {
	var input string

	cmd := &cobra.Command{
		Use:   "subscription",
		Short: "Validate and render JSON subscription templates",
	}

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate a v2 subscription template",
		RunE: func(_ *cobra.Command, _ []string) error {
			result, source, err := compileSubscriptionTemplate(input)
			if err != nil {
				return err
			}
			fmt.Printf("valid v2 subscription template: %d profile(s), %d auto-balancer(s) (%s)\n", result.ProfileCount, result.BalancerCount, source)
			return nil
		},
	}
	validate.Flags().StringVarP(&input, "input", "i", "", "Path to a v2 JSON subscription template, raw JSON, or '-'")
	_ = validate.MarkFlagRequired("input")

	var renderInput string
	var format string
	render := &cobra.Command{
		Use:   "render",
		Short: "Render a v2 subscription template as JSON, VLESS links, or Clash YAML",
		RunE: func(_ *cobra.Command, _ []string) error {
			result, _, err := compileSubscriptionTemplate(renderInput)
			if err != nil {
				return err
			}
			switch strings.ToLower(strings.TrimSpace(format)) {
			case "", "json":
				fmt.Print(result.JSONConfig)
			case "vless":
				links, err := convert.XrayJSONToShareText(result.ExportJSONConfig)
				if err != nil {
					return fmt.Errorf("render VLESS links: %w", err)
				}
				fmt.Print(links)
			case "clash":
				yaml, err := convert.XrayJSONToClashYAML(result.ExportJSONConfig)
				if err != nil {
					return fmt.Errorf("render Clash YAML: %w", err)
				}
				fmt.Print(yaml)
			default:
				return fmt.Errorf("unsupported format %q (supported: json, vless, clash)", format)
			}
			return nil
		},
	}
	render.Flags().StringVarP(&renderInput, "input", "i", "", "Path to a v2 JSON subscription template, raw JSON, or '-'")
	render.Flags().StringVarP(&format, "format", "f", "json", "Output format: json, vless, or clash")
	_ = render.MarkFlagRequired("input")

	cmd.AddCommand(validate, render)
	return cmd
}

func compileSubscriptionTemplate(input string) (pluginapi.SubscriptionTemplateResult, string, error) {
	raw, source, err := convert.ResolveInput(input)
	if err != nil {
		return pluginapi.SubscriptionTemplateResult{}, "", fmt.Errorf("read subscription template: %w", err)
	}
	result, err := autoBalancerPlugin.New().ProcessSubscriptionTemplate(context.Background(), raw)
	if err != nil {
		return pluginapi.SubscriptionTemplateResult{}, "", err
	}
	if !result.Handled {
		return pluginapi.SubscriptionTemplateResult{}, "", fmt.Errorf("%s is not a v2 subscription template (expected top-level \"version\": 2)", source)
	}
	return result, source, nil
}
