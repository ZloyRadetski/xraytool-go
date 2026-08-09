package engine_xray

import "xraytool/internal/xrayconfig"

// These aliases keep the Xray engine's public Go API source-compatible while
// locating the field-preserving config representation outside the plugin. Core
// can read legacy Xray templates without importing a concrete engine plugin.
type RawConfig = xrayconfig.RawConfig
type RawInbound = xrayconfig.RawInbound
type RawClient = xrayconfig.RawClient
type TaggedClient = xrayconfig.TaggedClient
type InboundInfo = xrayconfig.InboundInfo
type RealityKeys = xrayconfig.RealityKeys

var (
	GenerateRealityKeys     = xrayconfig.GenerateRealityKeys
	LoadOrCreateRealityKeys = xrayconfig.LoadOrCreateRealityKeys
	LoadRealityKeys         = xrayconfig.LoadRealityKeys
)
