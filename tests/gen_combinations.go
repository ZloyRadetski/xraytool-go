//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"xraytool/internal/xrayconfig"
)

func main() {
	protocols := []string{"vless", "vmess", "trojan", "shadowsocks", "hysteria2", "wireguard"}
	transports := []string{"tcp", "kcp", "ws", "http", "quic", "grpc", "xhttp", ""}
	securities := []string{"none", "tls", "reality", ""}

	outFile, err := os.Create("combinations_output.txt")
	if err != nil {
		panic(err)
	}
	defer outFile.Close()

	params := xrayconfig.ClientParams{
		Email:   "test@example.com",
		UUID:    "12345678-1234-1234-1234-123456789012",
		Auth:    "my-secret-password",
		Subfile: "sub12345",
		Expire:  "01.01.2030",
	}

	for _, p := range protocols {
		for _, t := range transports {
			for _, s := range securities {
				// Hysteria doesn't use streamSettings in the same way, but we will test it anyway.
				
				ibMap := map[string]interface{}{
					"protocol": p,
				}
				
				streamSettings := map[string]interface{}{}
				if t != "" {
					streamSettings["network"] = t
				}
				if s != "" {
					streamSettings["security"] = s
				}
				
				if len(streamSettings) > 0 {
					ibMap["streamSettings"] = streamSettings
				}

				ibData, _ := json.Marshal(ibMap)
				var ib xrayconfig.RawInbound
				json.Unmarshal(ibData, &ib)

				client, err := xrayconfig.BuildClient(ib, params)
				
				titleT := t
				if titleT == "" {
					titleT = "default(tcp)"
				}
				titleS := s
				if titleS == "" {
					titleS = "default(none)"
				}

				outFile.WriteString(fmt.Sprintf("%s %s %s:\n", p, titleT, titleS))
				
				if err != nil {
					outFile.WriteString(fmt.Sprintf("ERROR: %v\n\n", err))
				} else {
					clientJSON, _ := json.MarshalIndent(client, "", "  ")
					outFile.WriteString(string(clientJSON) + "\n\n")
				}
			}
		}
	}
	fmt.Println("File generated successfully.")
}
