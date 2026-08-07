package subscription

import (
	"context"
	"strings"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

// setEngineClientConfigContributor recognises the optional client-config
// extension on a domain.Engine.  domain.Engine remains intentionally small;
// engines that can produce client links advertise that capability separately
// through pluginapi.ClientConfigContributor.
func (c *CacheManager) setEngineClientConfigContributor(engine domain.Engine) {
	contributor, ok := any(engine).(pluginapi.ClientConfigContributor)
	if !ok {
		return
	}
	c.SetClientConfigContributor(contributor)
}

// SetClientConfigContributor installs the engine-agnostic link contributor.
// It is useful during the strangler migration when the cache is constructed
// before Plugin Host finishes assembling its MultiEngine. Passing nil restores
// the byte-for-byte legacy subscription renderer.
func (c *CacheManager) SetClientConfigContributor(contributor pluginapi.ClientConfigContributor) {
	c.mu.Lock()
	c.clientConfigContributor = contributor
	c.mu.Unlock()
}

// ClientConfigContributor returns the installed contributor, if any. It is
// deliberately exposed as the pluginapi contract rather than an Xray type so
// subscription never needs to import a concrete engine package.
func (c *CacheManager) ClientConfigContributor() pluginapi.ClientConfigContributor {
	c.mu.RLock()
	contributor, _ := c.clientConfigContributor.(pluginapi.ClientConfigContributor)
	c.mu.RUnlock()
	return contributor
}

// BuildClientLinks invokes the optional contributor without holding the cache
// lock. available differentiates an absent contributor from a contributor that
// correctly returns no links for this engine/user combination.
func (c *CacheManager) BuildClientLinks(
	ctx context.Context,
	user pluginapi.VPNUserConfig,
) (links []pluginapi.ClientLink, available bool, err error) {
	contributor := c.ClientConfigContributor()
	if contributor == nil {
		return nil, false, nil
	}
	links, err = contributor.BuildClientLinks(ctx, user)
	return links, true, err
}

func clientLinksText(links []pluginapi.ClientLink) string {
	lines := make([]string, 0, len(links))
	for _, link := range links {
		if uri := strings.TrimSpace(link.URI); uri != "" {
			lines = append(lines, uri)
		}
	}
	return strings.Join(lines, "\n")
}
