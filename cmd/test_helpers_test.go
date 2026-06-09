package cmd

import (
	"bytes"
	"os"
	"testing"
	"xraytool/internal/appconfig"

	"gopkg.in/yaml.v3"
)

// testingExit is used to catch osExit calls
var testingExitCode int
var exitCalled bool

func mockOsExit(code int) {
	testingExitCode = code
	exitCalled = true
	// panic to stop execution without actually exiting
	panic("exit")
}

func setupTest(t *testing.T) {
	osExit = mockOsExit
	exitCalled = false
	testingExitCode = 0
	currentGOOS = "linux"
	geteuid = func() int { return 0 }

	// Create a dummy config
	cfg = &appconfig.Config{
		Server: appconfig.ServerConf{
			Domain: "example.com",
		},
		Xray: appconfig.XrayConf{
			APIAddr: "127.0.0.1:10085",
		},
		Paths: appconfig.PathsConf{
			XrayConfig:   "test_xray_config.json",
			LimitedDB:    "test_limited.db",
			TemplatesDir: "test_templates",
			ServersJSON:  "test_servers.json",
			GeoIPDat:     "../tests/test_geoip.dat",
			GeositeDat:   "../tests/test_geosite.dat",
		},
	}

	// Create test_config.yaml
	yamlData, _ := yaml.Marshal(cfg)
	os.WriteFile("test_config.yaml", yamlData, 0644)
	cfgFile = "test_config.yaml"
}

func teardownTest() {
	osExit = os.Exit
	rootCmd.SetArgs(nil)
	os.Remove("test_config.yaml")
	os.Remove("test_xray_config.json")
	os.Remove("test_limited.db")
	os.RemoveAll("test_templates")
	os.Remove("test_servers.json")
}

func captureOutput(f func()) string {
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w

	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				if r != "exit" {
					panic(r)
				}
			}
		}()
		f()
	}()

	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String()
}
