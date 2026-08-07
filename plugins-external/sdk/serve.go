package pluginrpc

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	externalPluginServiceName = "xraytool.plugin.v1.ExternalPlugin"
	serviceProxyServiceName   = "xraytool.plugin.v1.ServiceProxy"
)

// Handshake is shared by the host and external plugin binaries. AutoMTLS is
// enabled by the host, while the magic cookie prevents an accidental launch
// of an unrelated executable.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "XRAYTOOL_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "xraytool-plugin-v1",
}

// Serve starts an external xraytool plugin process. A plugin main normally is
// only:
//
//	func main() { pluginrpc.Serve(&myPlugin{}) }
//
// Serve does not return during normal operation.
func Serve(impl Implementation) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins: plugin.PluginSet{
			PluginName: &serverPlugin{impl: impl},
		},
		GRPCServer: func(options []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(options...)
		},
	})
}

// serverPlugin is intentionally server-only. The xraytool host has its own
// client descriptor; exposing a generic SDK client would make callers assume
// arbitrary Go interfaces are transportable across the process boundary.
type serverPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	impl Implementation
}

func (p *serverPlugin) GRPCServer(broker *plugin.GRPCBroker, server *grpc.Server) error {
	if p.impl == nil {
		return fmt.Errorf("pluginrpc: plugin server implementation is nil")
	}
	registerExternalPluginServer(server, &externalServer{impl: p.impl, broker: broker})
	return nil
}

func (*serverPlugin) GRPCClient(context.Context, *plugin.GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, fmt.Errorf("pluginrpc: SDK plugin descriptor is server-only")
}

var _ plugin.GRPCPlugin = (*serverPlugin)(nil)

type externalPluginServer interface {
	Describe(context.Context, *emptypb.Empty) (*structpb.Struct, error)
	Init(context.Context, *structpb.Struct) (*emptypb.Empty, error)
	Start(context.Context, *emptypb.Empty) (*emptypb.Empty, error)
	Stop(context.Context, *emptypb.Empty) (*emptypb.Empty, error)
	Health(context.Context, *emptypb.Empty) (*structpb.Struct, error)
	Call(context.Context, *structpb.Struct) (*structpb.Struct, error)
}

type externalServer struct {
	impl   Implementation
	broker *plugin.GRPCBroker
}

func (s *externalServer) Describe(ctx context.Context, _ *emptypb.Empty) (*structpb.Struct, error) {
	metadata, err := s.impl.Describe(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	result, err := toStruct(metadata)
	if err != nil {
		return nil, rpcError(err)
	}
	return result, nil
}

func (s *externalServer) Init(ctx context.Context, request *structpb.Struct) (*emptypb.Empty, error) {
	data := structMap(request)
	config, _ := data["config"].(map[string]any)
	proxyID, err := numberToUint32(data["service_proxy_broker_id"])
	if err != nil {
		return nil, rpcError(err)
	}

	init := InitRequest{Config: config}
	if proxyID != 0 {
		if s.broker == nil {
			return nil, status.Error(codes.FailedPrecondition, "pluginrpc: service proxy broker is unavailable")
		}
		conn, err := s.broker.Dial(proxyID)
		if err != nil {
			return nil, rpcError(fmt.Errorf("dial host service proxy: %w", err))
		}
		init.Services = &serviceProxyClient{conn: conn}
	}
	if err := s.impl.Init(ctx, init); err != nil {
		return nil, rpcError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *externalServer) Start(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	if err := s.impl.Start(ctx); err != nil {
		return nil, rpcError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *externalServer) Stop(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	if err := s.impl.Stop(ctx); err != nil {
		return nil, rpcError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *externalServer) Health(ctx context.Context, _ *emptypb.Empty) (*structpb.Struct, error) {
	if err := s.impl.Health(ctx); err != nil {
		result, encodeErr := toStruct(map[string]any{"healthy": false, "error": err.Error()})
		if encodeErr != nil {
			return nil, rpcError(encodeErr)
		}
		return result, nil
	}
	result, err := toStruct(map[string]any{"healthy": true})
	if err != nil {
		return nil, rpcError(err)
	}
	return result, nil
}

func (s *externalServer) Call(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	var call CallRequest
	if err := fromStruct(request, &call); err != nil {
		return nil, rpcError(err)
	}
	if err := ValidateCallRequest(call); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := s.impl.Call(ctx, call)
	if err != nil {
		return nil, rpcError(err)
	}
	response, err := toStruct(result)
	if err != nil {
		return nil, rpcError(err)
	}
	return response, nil
}

func numberToUint32(value any) (uint32, error) {
	if value == nil {
		return 0, nil
	}
	floatValue, ok := value.(float64)
	if !ok || floatValue < 0 || floatValue > float64(^uint32(0)) || floatValue != float64(uint32(floatValue)) {
		return 0, fmt.Errorf("pluginrpc: service_proxy_broker_id must be an unsigned integer")
	}
	return uint32(floatValue), nil
}

type serviceProxyClient struct {
	conn *grpc.ClientConn
}

func (c *serviceProxyClient) Call(ctx context.Context, req CallRequest) (CallResponse, error) {
	if err := ValidateCallRequest(req); err != nil {
		return CallResponse{}, err
	}
	request, err := toStruct(req)
	if err != nil {
		return CallResponse{}, err
	}
	response := new(structpb.Struct)
	if err := c.conn.Invoke(ctx, "/"+serviceProxyServiceName+"/Call", request, response); err != nil {
		return CallResponse{}, fmt.Errorf("host service proxy RPC: %w", err)
	}
	var result CallResponse
	if err := fromStruct(response, &result); err != nil {
		return CallResponse{}, err
	}
	return result, nil
}

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

func registerExternalPluginServer(server *grpc.Server, impl externalPluginServer) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: externalPluginServiceName,
		HandlerType: (*externalPluginServer)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Describe", Handler: describeHandler},
			{MethodName: "Init", Handler: initHandler},
			{MethodName: "Start", Handler: startHandler},
			{MethodName: "Stop", Handler: stopHandler},
			{MethodName: "Health", Handler: healthHandler},
			{MethodName: "Call", Handler: callHandler},
		},
	}, impl)
}

func describeHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(emptypb.Empty)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(externalPluginServer).Describe(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + externalPluginServiceName + "/Describe"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(externalPluginServer).Describe(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, request, info, handler)
}

func initHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(structpb.Struct)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(externalPluginServer).Init(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + externalPluginServiceName + "/Init"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(externalPluginServer).Init(ctx, req.(*structpb.Struct))
	}
	return interceptor(ctx, request, info, handler)
}

func startHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(emptypb.Empty)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(externalPluginServer).Start(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + externalPluginServiceName + "/Start"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(externalPluginServer).Start(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, request, info, handler)
}

func stopHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(emptypb.Empty)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(externalPluginServer).Stop(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + externalPluginServiceName + "/Stop"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(externalPluginServer).Stop(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, request, info, handler)
}

func healthHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(emptypb.Empty)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(externalPluginServer).Health(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + externalPluginServiceName + "/Health"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(externalPluginServer).Health(ctx, req.(*emptypb.Empty))
	}
	return interceptor(ctx, request, info, handler)
}

func callHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(structpb.Struct)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(externalPluginServer).Call(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + externalPluginServiceName + "/Call"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(externalPluginServer).Call(ctx, req.(*structpb.Struct))
	}
	return interceptor(ctx, request, info, handler)
}
