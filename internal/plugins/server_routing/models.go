package server_routing

// RoutingRule represents a single routing rule on a server.
type RoutingRule struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	SourceServer string   `json:"source_server"`
	TargetServer string   `json:"target_server"`
	Domain       []string `json:"domain"`
	IP           []string `json:"ip"`
	Priority     int      `json:"priority"`
	Enabled      bool     `json:"enabled"`
}

// ServerRoutingConfig is the per-server routing file (<server-name>.json).
type ServerRoutingConfig struct {
	Server string        `json:"server"`
	Rules  []RoutingRule `json:"rules"`
}

// ServerNode describes a server node for the topology response.
type ServerNode struct {
	Name     string `json:"name"`
	IsMaster bool   `json:"is_master"`
	IP       string `json:"ip"`
	Domain   string `json:"domain"`
	Online   bool   `json:"online"`
}

// SpecialNode represents DIRECT or BLOCK pseudo-targets on the map.
type SpecialNode struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TopologyResponse is the full state returned to the frontend.
type TopologyResponse struct {
	Servers      []ServerNode          `json:"servers"`
	SpecialNodes []SpecialNode         `json:"special_nodes"`
	Routing      []ServerRoutingConfig `json:"routing"`
	Outbounds    []string              `json:"outbounds"`
}

// ApplyRequest is sent by the frontend to save and apply changes.
type ApplyRequest struct {
	Routing []ServerRoutingConfig `json:"routing"`
}

// ApplyResponse confirms the result of applying routing.
type ApplyResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// ValidationError indicates a business-rule or data validation failure.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}

