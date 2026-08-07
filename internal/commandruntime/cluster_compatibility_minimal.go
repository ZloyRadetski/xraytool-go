//go:build minimal

package commandruntime

import (
	"context"

	"xraytool/internal/domain"
)

// Minimal builds intentionally omit legacy slave/state-sync transport.
func configureClusterCompatibility(*Dependencies) {}

func (*Dependencies) StartFraudReporter(context.Context) domain.FraudEventReporter { return nil }

func (*Dependencies) SyncServiceFor(domain.Engine) SyncService { return nil }
