//go:build ignore

package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	content, err := os.ReadFile("combinations_output.txt")
	if err != nil {
		panic(err)
	}

	blocks := strings.Split(string(content), "\n\n")
	errorsFound := 0

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		lines := strings.Split(block, "\n")
		header := lines[0]
		header = strings.TrimSuffix(header, ":")
		parts := strings.Split(header, " ")
		if len(parts) != 3 {
			continue
		}

		protocol := parts[0]
		transport := parts[1]
		security := parts[2]

		isTCP := transport == "tcp" || transport == "default(tcp)"
		isTLSorReality := security == "tls" || security == "reality"
		hasVision := strings.Contains(block, "xtls-rprx-vision")
		hasError := strings.Contains(block, "ERROR:")

		// Rules
		if protocol == "wireguard" {
			if !hasError {
				fmt.Printf("FAIL: %s should have an error\n", header)
				errorsFound++
			}
			continue
		}

		if hasError {
			fmt.Printf("FAIL: %s unexpectedly failed\n", header)
			errorsFound++
			continue
		}

		// XTLS Vision Rule
		if protocol == "vless" && isTCP && isTLSorReality {
			if !hasVision {
				fmt.Printf("FAIL: %s should have xtls-rprx-vision\n", header)
				errorsFound++
			}
		} else {
			if hasVision {
				fmt.Printf("FAIL: %s should NOT have xtls-rprx-vision\n", header)
				errorsFound++
			}
		}

		// Field checks
		if protocol == "vless" || protocol == "vmess" {
			if !strings.Contains(block, `"id"`) {
				fmt.Printf("FAIL: %s missing id\n", header)
				errorsFound++
			}
		}
		if protocol == "trojan" || protocol == "shadowsocks" {
			if !strings.Contains(block, `"password"`) {
				fmt.Printf("FAIL: %s missing password\n", header)
				errorsFound++
			}
		}
		if protocol == "hysteria2" {
			if !strings.Contains(block, `"auth"`) {
				fmt.Printf("FAIL: %s missing auth\n", header)
				errorsFound++
			}
		}
	}

	if errorsFound == 0 {
		fmt.Println("SUCCESS: All combinations are correct!")
	} else {
		fmt.Printf("FAILED: Found %d errors\n", errorsFound)
	}
}
