package subscription

import (
	"context"
	"fmt"

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
	var errs []string

	addErr := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	// 1. Hot-Remove explicitly removed users in bulk (exactly 1 config file write)
	if len(payload.Remove) > 0 {
		addErr(engine.RemoveUsersBulk(context.Background(), payload.Remove))
	}

	// 2. Hot-Add/Update:
	// To prevent "already exists" errors on updates, we first remove the users we are about to add/update,
	// and then add them in bulk.
	if len(payload.Add) > 0 {
		addEmails := make([]string, len(payload.Add))
		for i, u := range payload.Add {
			addEmails[i] = u.Email
		}
		// 2a. Bulk remove the users we are going to add (ensures clean update)
		_ = engine.RemoveUsersBulk(context.Background(), addEmails)

		// 2b. Bulk add the new/updated users (exactly 1 config file write)
		addErr(engine.AddUsersBulk(context.Background(), payload.Add))
	}

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
