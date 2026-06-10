package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"xraytool/internal/appconfig"
)

type PathCheck struct {
	Name       string
	Path       string
	NeedRoot   string // "R" or "RW" or "-"
	NeedWeb    string // "R" or "RW" or "-"
	StatusRoot string // "OK", "Missing", "No Read", "No Write", "Parent No Write"
	StatusWeb  string // "OK", "Missing", "No Read", "No Write", "Parent No Write"
	FixCmds    []string
}

func checkAndReportPermissions() {
	configPath := cfgFile
	if configPath == "" {
		configPath = "/etc/xraytool/config.yaml"
	}

	webUID, webGID, webUser := getWebUser()

	// Load config for checking
	localCfg, cfgErr := appconfig.Load(configPath)

	var checks []PathCheck

	// 1. Check config file itself
	cCheck := checkPath("xraytool config file", configPath, "RW", "R", false, webUID, webGID, webUser)
	if cfgErr != nil {
		cCheck.StatusRoot = "Error Loading"
		cCheck.StatusWeb = "Unknown"
		cCheck.FixCmds = []string{
			"sudo mkdir -p /etc/xraytool",
			"sudo touch " + configPath,
			"sudo chown -R root:" + webUser + " /etc/xraytool",
			"sudo chmod 750 /etc/xraytool",
			"sudo chmod 640 " + configPath,
		}
	}
	checks = append(checks, cCheck)

	if localCfg != nil {
		// Helper to resolve template paths
		resolvePath := func(primary string, suffix string) string {
			if _, err := os.Stat(primary); err == nil {
				return primary
			}
			devLower := strings.ReplaceAll(primary, "/helpful_bots/Dev/", "/helpful_bots/dev/")
			if _, err := os.Stat(devLower); err == nil {
				return devLower
			}
			fallbacks := []string{
				"/var/www/TorvaldsVPN/helpful_bots/dev/" + suffix,
				"/var/www/TorvaldsVPN/helpful_bots/" + suffix,
				"/var/www/TorvaldsVPN/xraytool/" + suffix,
				"./" + suffix,
			}
			for _, fb := range fallbacks {
				if _, err := os.Stat(fb); err == nil {
					return fb
				}
			}
			return primary
		}

		pathsToCheck := []struct {
			name        string
			path        string
			needRoot    string
			needWeb     string
			isDirectory bool
		}{
			{"Xray main config", localCfg.Paths.XrayConfig, "RW", "R", false},
			{"Limited users database", localCfg.Paths.LimitedDB, "RW", "R", false},
			{"Inferred stats", localCfg.Paths.InferredStats, "RW", "R", false},
			{"Stats state file", localCfg.Paths.StatsState, "RW", "-", false},
			{"Servers JSON file", localCfg.Paths.ServersJSON, "RW", "-", false},
			{"Devices state file", localCfg.Paths.DevicesState, "RW", "RW", false},
			{"Subscription template", resolvePath(localCfg.Paths.JSONSubscriptionTemplate, "configs.txt"), "RW", "R", false},
			{"Routing template", resolvePath(localCfg.Paths.RoutingTemplate, "routing.json"), "RW", "R", false},
			{"Routing RU template", resolvePath(localCfg.Paths.RoutingRUTemplate, "routing_ALL_RU.json"), "RW", "R", false},
			{"GeoIP database", localCfg.Paths.GeoIPDat, "RW", "-", false},
			{"Geosite database", localCfg.Paths.GeositeDat, "RW", "-", false},
		}

		for _, pt := range pathsToCheck {
			checks = append(checks, checkPath(pt.name, pt.path, pt.needRoot, pt.needWeb, pt.isDirectory, webUID, webGID, webUser))
		}
	}

	// Filter checks that have issues
	var issues []PathCheck
	for _, c := range checks {
		if c.StatusRoot != "OK" || (c.NeedWeb != "-" && c.StatusWeb != "OK") {
			issues = append(issues, c)
		}
	}

	if len(issues) == 0 {
		return
	}

	// Print warning banner
	fmt.Println("\n\033[1;31m⚠️  WARNING: FILESYSTEM PERMISSIONS ALERTS  ⚠️\033[0m")
	fmt.Printf("Some files or directories have incorrect permissions for root or web user '%s'.\n", webUser)
	fmt.Println("This will prevent user management commands or subscription updates from functioning.")
	fmt.Println()

	// Collect fix commands (deduplicated)
	var fixCmds []string
	seenCmds := make(map[string]bool)
	addCmds := func(cmds []string) {
		for _, cmd := range cmds {
			if !seenCmds[cmd] {
				seenCmds[cmd] = true
				fixCmds = append(fixCmds, cmd)
			}
		}
	}

	for _, issue := range issues {
		fmt.Printf("  \033[1;33m• [%s]\033[0m %s\n", issue.Name, filepath.ToSlash(issue.Path))
		if issue.StatusRoot != "OK" {
			fmt.Printf("    - Root:      \033[0;31m%s\033[0m (Needs %s)\n", issue.StatusRoot, issue.NeedRoot)
		}
		if issue.NeedWeb != "-" && issue.StatusWeb != "OK" {
			fmt.Printf("    - Web (%s): \033[0;31m%s\033[0m (Needs %s)\n", webUser, issue.StatusWeb, issue.NeedWeb)
		}
		addCmds(issue.FixCmds)
	}

	fmt.Println()
	fmt.Println("\033[1;32m👉 RUN THESE COMMANDS AS ROOT TO FIX THE ISSUES:\033[0m")
	for _, cmd := range fixCmds {
		fmt.Printf("  %s\n", cmd)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("-", 60))
}

func checkPath(name, path, needRoot, needWeb string, isDir bool, webUID, webGID int, webUser string) PathCheck {
	check := PathCheck{
		Name:       name,
		Path:       path,
		NeedRoot:   needRoot,
		NeedWeb:    needWeb,
		StatusRoot: "OK",
		StatusWeb:  "OK",
	}

	if path == "" {
		check.StatusRoot = "Not Configured"
		check.StatusWeb = "Not Configured"
		return check
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			check.StatusRoot = "Missing"
			check.StatusWeb = "Missing"

			// Check if parent directory is writable so it can be created
			parent := filepath.Dir(path)
			pInfo, pErr := os.Stat(parent)
			if pErr != nil {
				check.StatusRoot = "Parent Missing"
				check.StatusWeb = "Parent Missing"
				check.FixCmds = append(check.FixCmds, "sudo mkdir -p "+filepath.ToSlash(parent))
			} else {
				_, rootW := checkUnixAccess(pInfo, 0, 0)
				_, webW := checkUnixAccess(pInfo, webUID, webGID)

				if needRoot == "RW" && !rootW {
					check.StatusRoot = "Parent No Write"
					check.FixCmds = append(check.FixCmds, "sudo chmod 755 "+filepath.ToSlash(parent))
				}
				if needWeb == "RW" && !webW {
					check.StatusWeb = "Parent No Write"
					check.FixCmds = append(check.FixCmds, "sudo chown :"+webUser+" "+filepath.ToSlash(parent), "sudo chmod 775 "+filepath.ToSlash(parent))
				}
			}
			return check
		}
		check.StatusRoot = "Error: " + err.Error()
		check.StatusWeb = "Error: " + err.Error()
		return check
	}

	if isDir && !info.IsDir() {
		check.StatusRoot = "Not a Directory"
		check.StatusWeb = "Not a Directory"
		return check
	}

	// Check access for root (UID 0, GID 0)
	rootR, rootW := checkUnixAccess(info, 0, 0)
	if needRoot == "R" && !rootR {
		check.StatusRoot = "No Read"
	} else if needRoot == "RW" && (!rootR || !rootW) {
		check.StatusRoot = "No Read/Write"
	}

	if check.StatusRoot != "OK" {
		if isDir {
			check.FixCmds = append(check.FixCmds, "sudo chmod 755 "+filepath.ToSlash(path))
		} else {
			check.FixCmds = append(check.FixCmds, "sudo chmod 644 "+filepath.ToSlash(path))
		}
	}

	// Check access for web user
	if needWeb != "-" {
		webR, webW := checkUnixAccess(info, webUID, webGID)
		if needWeb == "R" && !webR {
			check.StatusWeb = "No Read"
			if isDir {
				check.FixCmds = append(check.FixCmds, "sudo chown -R root:"+webUser+" "+filepath.ToSlash(path), "sudo chmod 750 "+filepath.ToSlash(path))
			} else {
				check.FixCmds = append(check.FixCmds, "sudo chown root:"+webUser+" "+filepath.ToSlash(path), "sudo chmod 640 "+filepath.ToSlash(path))
			}
		} else if needWeb == "RW" && (!webR || !webW) {
			check.StatusWeb = "No Read/Write"
			if isDir {
				check.FixCmds = append(check.FixCmds, "sudo chown -R root:"+webUser+" "+filepath.ToSlash(path), "sudo chmod 770 "+filepath.ToSlash(path))
			} else {
				check.FixCmds = append(check.FixCmds, "sudo chown root:"+webUser+" "+filepath.ToSlash(path), "sudo chmod 660 "+filepath.ToSlash(path))
			}
		}
	}

	return check
}
