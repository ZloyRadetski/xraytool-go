package pluginrpc

import (
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// Handshake is shared by the host and every external plugin binary. The magic
// cookie prevents accidentally launching a random executable; AutoMTLS on the
// host client authenticates the actual RPC transport.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "XRAYTOOL_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "xraytool-plugin-v1",
}

// Serve starts an external xraytool plugin process. A plugin main typically is
// just:
//
//	func main() { pluginrpc.Serve(&myPlugin{}) }
//
// Serve does not return under normal operation.
func Serve(impl Implementation) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins: plugin.PluginSet{
			PluginName: &Plugin{Impl: impl},
		},
		GRPCServer: func(options []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(options...)
		},
	})
}
