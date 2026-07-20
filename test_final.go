package main

import (
	"fmt"
	"xraytool/internal/convert"
)

func main() {
	jsonStr := `{
		"outbounds": [{
			"protocol": "vless",
			"settings": {
				"vnext": [{
					"address": "example.com",
					"port": 443,
					"users": [{"id": "00000000-0000-0000-0000-000000000000"}]
				}]
			},
			"streamSettings": {
				"network": "xhttp",
				"xhttpSettings": {
					"host": "",
					"path": "/my-path",
					"mode": "",
					"headers": null,
					"xPaddingBytes": "0",
					"xPaddingObfsMode": true,
					"xPaddingKey": "_dc",
					"xPaddingHeader": "X-Cache",
					"xPaddingPlacement": "queryInHeader",
					"xPaddingMethod": "tokenish",
					"uplinkHTTPMethod": "POST",
					"noGRPCHeader": false,
					"noSSEHeader": false,
					"scMaxEachPostBytes": "0",
					"xmux": {
						"maxConcurrency": "0"
					},
					"downloadSettings": null,
					"extra": null
				}
			}
		}]
	}`

	links, _ := convert.XrayJSONToShareText(jsonStr)
	fmt.Println(links)
}
