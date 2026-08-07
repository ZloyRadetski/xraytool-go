package referral

import (
	"context"
	"fmt"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

type Plugin struct {
	log      pluginapi.Logger
	registry domain.Registry
}

func NewPlugin() *Plugin {
	return &Plugin{}
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "referral",
		Kind:        "event_sink",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Referral reward manager",
		Requires: []pluginapi.ServiceRef{
			{Name: "domain_registry"},
		},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = reg.Logger()

	domainReg, err := reg.Resolve("domain_registry")
	if err != nil {
		return err
	}
	p.registry = domainReg.(domain.Registry)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return nil
}

func (p *Plugin) Health(ctx context.Context) error {
	return nil
}

func (p *Plugin) Handle(ctx context.Context, ev pluginapi.Event) error {
	if ev.Type == "payment.completed" {
		var paymentID int64
		switch v := ev.Data["payment_id"].(type) {
		case float64:
			paymentID = int64(v)
		case int64:
			paymentID = v
		case int:
			paymentID = int64(v)
		default:
			return nil
		}

		// Get the payment
		payment, err := p.registry.Payments().FindByID(ctx, fmt.Sprintf("%d", paymentID))
		if err != nil || payment == nil {
			return err
		}

		user, err := p.registry.Users().FindByID(ctx, payment.UserID)
		if err != nil || user == nil || user.ReferredBy == nil {
			return nil
		}

		reward := payment.Amount / 4
		if reward > 0 {
			if err := p.registry.Users().AddReferralReward(ctx, *user.ReferredBy, user.ID, payment.ID, reward); err != nil {
				p.log.Error("failed to apply referral reward", "payment_id", payment.ID, "err", err)
				return err
			}
			p.log.Info("applied referral reward", "payment_id", payment.ID, "reward", reward, "referrer", *user.ReferredBy)
		}
	}
	return nil
}
