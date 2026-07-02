package subscription

import (
	"context"
	"fmt"
	"sync"

		"xraytool/internal/domain"
)

type BatchApplyResult struct {
	Ok      bool   `json:"ok"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Error   string `json:"error,omitempty"`
}

// ApplyBatchOperations executes a batch of Add/Remove user operations on the local node.
func ApplyBatchOperations(engine domain.Engine, payload domain.BatchPayload) BatchApplyResult {
	var wg sync.WaitGroup
	var errs []string
	var mu sync.Mutex

	addErr := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, err.Error())
			mu.Unlock()
		}
	}

	// 1. Hot-Remove explicitly removed users
	for _, email := range payload.Remove {
		wg.Add(1)
		go func(e string) {
			defer wg.Done()
			addErr(engine.BanUser(context.Background(), e))
		}(email)
	}

	// Wait for all removals to finish before adding
	wg.Wait()

	// 2. Hot-Add/Update
	for _, u := range payload.Add {
		wg.Add(1)
		go func(userCfg domain.VPNUserConfig) {
			defer wg.Done()

			// Remove first to prevent "already exists" errors on update, ignore errors.
			_ = engine.BanUser(context.Background(), userCfg.Email)

			
			
			addErr(engine.AddUser(context.Background(), userCfg))
		}(u)
	}

	wg.Wait()

	if len(errs) > 0 {
		return BatchApplyResult{Ok: false, Error: fmt.Sprintf("engine errors: %v", errs)}
	}

	return BatchApplyResult{
		Ok:      true,
		Status:  "ok",
		Added:   len(payload.Add),
		Removed: len(payload.Remove),
	}
}
