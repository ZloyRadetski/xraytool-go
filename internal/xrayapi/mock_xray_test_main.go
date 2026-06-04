package xrayapi

import (
	"fmt"
	"os"
)

func main() {
	if os.Getenv("MOCK_XRAY_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "Mock error output")
		os.Exit(1)
	}

	if os.Getenv("MOCK_XRAY_STATS") == "1" {
		fmt.Println(`{ "stat": [ { "name": "user>>>test@example.com>>>traffic>>>uplink", "value": 123 } ] }`)
		os.Exit(0)
	}

	if os.Getenv("MOCK_XRAY_STATS_EMPTY") == "1" {
		fmt.Println(`null`)
		os.Exit(0)
	}
    
    if os.Getenv("MOCK_XRAY_STATS_BAD_JSON") == "1" {
		fmt.Println(`{ bad json`)
		os.Exit(0)
	}

	// For any other successful case
	fmt.Println("Success")
	os.Exit(0)
}
