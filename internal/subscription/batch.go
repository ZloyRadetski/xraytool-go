package subscription

import (
	"fmt"
	"sync"

	"xraytool/internal/appconfig"
	"xraytool/internal/slave"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

type BatchApplyResult struct {
	Ok      bool   `json:"ok"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Error   string `json:"error,omitempty"`
}

// ApplyBatchOperations executes a batch of Add/Remove user operations on the local node.
func ApplyBatchOperations(cfg *appconfig.Config, payload slave.BatchPayload) BatchApplyResult {
	xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
	if err != nil {
		return BatchApplyResult{Ok: false, Error: fmt.Sprintf("reading config: %v", err)}
	}
	
	// Keep a clean copy of original config for tag extraction during Hot-Remove
	originalCfg, _ := xrayconfig.Read(cfg.Paths.XrayConfig)

	// Apply Removes
	if len(payload.Remove) > 0 {
		_ = xrayconfig.RemoveUsersFromAllInbounds(xrayCfg, payload.Remove)
	}

	// Apply Adds
	var addEmails []string
	for _, u := range payload.Add {
		addEmails = append(addEmails, u.Email)
	}
	if len(addEmails) > 0 {
		_ = xrayconfig.RemoveUsersFromAllInbounds(xrayCfg, addEmails)
	}

	for _, u := range payload.Add {
		params := xrayconfig.ClientParams{
			Email:   u.Email,
			UUID:    u.UUID,
			Auth:    u.Auth,
			Subfile: u.Subfile,
			Expire:  u.Expire,
			Limit:   u.Limit,
		}
		tagged, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
		if err == nil && len(tagged) > 0 {
			_ = xrayconfig.AddUserToInbounds(xrayCfg, tagged)
		}
	}

	// Write config
	if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return BatchApplyResult{Ok: false, Error: fmt.Sprintf("writing config: %v", err)}
	}

	// Apply Hot-Reload using Xray API
	apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
	
	var wg sync.WaitGroup

	// 1. Hot-Remove
	tagsMap, _ := xrayconfig.InboundTagsForUsers(originalCfg, payload.Remove)
	for _, email := range payload.Remove {
		tags := tagsMap[email]
		wg.Add(1)
		go func(e string, t []string) {
			defer wg.Done()
			_ = apiClient.RemoveUser(e, t)
		}(email, tags)
	}
	
	// 2. Hot-Add/Update
	for _, u := range payload.Add {
		params := xrayconfig.ClientParams{
			Email:   u.Email,
			UUID:    u.UUID,
			Auth:    u.Auth,
			Subfile: u.Subfile,
			Expire:  u.Expire,
			Limit:   u.Limit,
		}
		tagged, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
		if err == nil && len(tagged) > 0 {
			wg.Add(1)
			go func(tg []xrayconfig.TaggedClient) {
				defer wg.Done()
				_ = apiClient.AddUser(tg, cfg.Paths.XrayConfig)
			}(tagged)
		}
	}

	wg.Wait()

	return BatchApplyResult{
		Ok:      true,
		Status:  "success",
		Added:   len(payload.Add),
		Removed: len(payload.Remove),
	}
}
