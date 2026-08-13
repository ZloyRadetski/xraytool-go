package pluginrpc

import (
	"context"
	"fmt"
	"sync"

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

// Plugin is the go-plugin adapter shared by the host and external binaries.
// Set Impl on the plugin process side; leave it nil on the host side.
type Plugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl Implementation
}

// GRPCServer registers the external plugin API in a plugin subprocess.
func (p *Plugin) GRPCServer(broker *plugin.GRPCBroker, server *grpc.Server) error {
	if p.Impl == nil {
		return fmt.Errorf("pluginrpc: plugin server implementation is nil")
	}
	registerExternalPluginServer(server, &externalServer{impl: p.Impl, broker: broker})
	return nil
}

// GRPCClient creates the host-side RPC client. The returned Client is safe for
// concurrent calls; go-plugin cancels ctx if the plugin subprocess exits.
func (*Plugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, conn *grpc.ClientConn) (interface{}, error) {
	return &Client{ctx: ctx, broker: broker, conn: conn}, nil
}

var _ plugin.GRPCPlugin = (*Plugin)(nil)

// ClientPlugin returns the go-plugin descriptor used by the host's ClientConfig.
func ClientPlugin() plugin.Plugin { return &Plugin{} }

// Client is a typed client to one external plugin process.
type Client struct {
	ctx    context.Context
	broker *plugin.GRPCBroker
	conn   *grpc.ClientConn
}

// Describe obtains metadata before the host validates the dependency graph.
func (c *Client) Describe(ctx context.Context) (Metadata, error) {
	response := new(structpb.Struct)
	if err := c.invoke(ctx, "Describe", &emptypb.Empty{}, response); err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	if err := fromStruct(response, &metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

// Init passes plugin configuration and a brokered host service proxy to the
// external process. proxyID is zero when no services are required.
func (c *Client) Init(ctx context.Context, config map[string]any, proxyID uint32, required []string) error {
	request, err := toStruct(map[string]any{
		"config":                  config,
		"service_proxy_broker_id": proxyID,
		"required_services":       required,
	})
	if err != nil {
		return err
	}
	return c.invoke(ctx, "Init", request, &emptypb.Empty{})
}

// Start invokes the long-running lifecycle call.
func (c *Client) Start(ctx context.Context) error {
	return c.invoke(ctx, "Start", &emptypb.Empty{}, &emptypb.Empty{})
}

// Stop asks the plugin to shut down gracefully.
func (c *Client) Stop(ctx context.Context) error {
	return c.invoke(ctx, "Stop", &emptypb.Empty{}, &emptypb.Empty{})
}

// Health checks the plugin's readiness/liveness endpoint.
func (c *Client) Health(ctx context.Context) error {
	response := new(structpb.Struct)
	if err := c.invoke(ctx, "Health", &emptypb.Empty{}, response); err != nil {
		return err
	}
	var result struct {
		Healthy bool   `json:"healthy"`
		Error   string `json:"error"`
	}
	if err := fromStruct(response, &result); err != nil {
		return err
	}
	if !result.Healthy {
		if result.Error == "" {
			result.Error = "plugin reported unhealthy"
		}
		return fmt.Errorf("%s", result.Error)
	}
	return nil
}

// Call invokes an explicit structured operation on the external plugin.
func (c *Client) Call(ctx context.Context, req CallRequest) (CallResponse, error) {
	if err := ValidateCallRequest(req); err != nil {
		return CallResponse{}, err
	}
	request, err := toStruct(req)
	if err != nil {
		return CallResponse{}, err
	}
	response := new(structpb.Struct)
	if err := c.invoke(ctx, "Call", request, response); err != nil {
		return CallResponse{}, err
	}
	var result CallResponse
	if err := fromStruct(response, &result); err != nil {
		return CallResponse{}, err
	}
	return result, nil
}

// OpenServiceProxy starts a host-side brokered ServiceProxy server and returns
// the broker ID that must be supplied to Init. Each handler is deliberately
// explicit; values that merely happen to be Go interfaces cannot cross this
// boundary.
func (c *Client) OpenServiceProxy(handlers map[string]ServiceHandler) (uint32, error) {
	if c == nil || c.broker == nil {
		return 0, fmt.Errorf("pluginrpc: service proxy is unavailable before plugin connection")
	}
	id := c.broker.NextId()
	copyHandlers := make(map[string]ServiceHandler, len(handlers))
	for name, handler := range handlers {
		if name == "" {
			return 0, fmt.Errorf("pluginrpc: service proxy contains an empty service name")
		}
		if handler == nil {
			return 0, fmt.Errorf("pluginrpc: service proxy handler %q is nil", name)
		}
		copyHandlers[name] = handler
	}
	go c.broker.AcceptAndServe(id, func(options []grpc.ServerOption) *grpc.Server {
		server := grpc.NewServer(options...)
		registerServiceProxyServer(server, &serviceProxyServer{handlers: copyHandlers})
		return server
	})
	return id, nil
}

func (c *Client) invoke(ctx context.Context, method string, request, response any) error {
	if c == nil || c.conn == nil {
		return fmt.Errorf("pluginrpc: client is not connected")
	}
	if ctx == nil {
		return fmt.Errorf("pluginrpc: RPC context must not be nil")
	}
	if c.ctx != nil {
		select {
		case <-c.ctx.Done():
			return fmt.Errorf("external plugin process exited: %w", c.ctx.Err())
		default:
		}
	}
	if err := c.conn.Invoke(ctx, "/"+externalPluginServiceName+"/"+method, request, response); err != nil {
		return fmt.Errorf("external plugin %s RPC: %w", method, err)
	}
	return nil
}

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

	proxyMu sync.Mutex
	proxy   *serviceProxyClient
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
	var serviceProxy *serviceProxyClient
	if proxyID != 0 {
		if s.broker == nil {
			return nil, status.Error(codes.FailedPrecondition, "pluginrpc: service proxy broker is unavailable")
		}
		conn, err := s.broker.Dial(proxyID)
		if err != nil {
			return nil, rpcError(fmt.Errorf("dial host service proxy: %w", err))
		}
		serviceProxy = &serviceProxyClient{conn: conn}
		init.Services = serviceProxy
	}
	if err := s.impl.Init(ctx, init); err != nil {
		_ = serviceProxy.Close()
		return nil, rpcError(err)
	}
	s.replaceServiceProxy(serviceProxy)
	return &emptypb.Empty{}, nil
}

func (s *externalServer) Start(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	if err := s.impl.Start(ctx); err != nil {
		return nil, rpcError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *externalServer) Stop(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	defer s.closeServiceProxy()
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

type serviceProxyService interface {
	Call(context.Context, *structpb.Struct) (*structpb.Struct, error)
}

type serviceProxyServer struct {
	handlers map[string]ServiceHandler
}

func (s *serviceProxyServer) Call(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	var call CallRequest
	if err := fromStruct(request, &call); err != nil {
		return nil, rpcError(err)
	}
	if err := ValidateCallRequest(call); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	handler, ok := s.handlers[call.Service]
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "external plugin is not allowed to call host service %q", call.Service)
	}
	result, err := handler.Call(ctx, call)
	if err != nil {
		return nil, rpcError(fmt.Errorf("host service %q method %q: %w", call.Service, call.Method, err))
	}
	response, err := toStruct(result)
	if err != nil {
		return nil, rpcError(err)
	}
	return response, nil
}

type serviceProxyClient struct {
	conn *grpc.ClientConn
}

// Close releases the brokered gRPC connection created during Init. The server
// owns this connection because it is created on behalf of an Implementation;
// without explicit cleanup every failed init/restart leaks HTTP/2 goroutines.
func (c *serviceProxyClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
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

func (s *externalServer) replaceServiceProxy(next *serviceProxyClient) {
	s.proxyMu.Lock()
	previous := s.proxy
	s.proxy = next
	s.proxyMu.Unlock()
	if previous != nil && previous != next {
		_ = previous.Close()
	}
}

func (s *externalServer) closeServiceProxy() {
	s.replaceServiceProxy(nil)
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

func registerServiceProxyServer(server *grpc.Server, impl serviceProxyService) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: serviceProxyServiceName,
		HandlerType: (*serviceProxyService)(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "Call", Handler: proxyCallHandler}},
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

func proxyCallHandler(srv interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	request := new(structpb.Struct)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(serviceProxyService).Call(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + serviceProxyServiceName + "/Call"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(serviceProxyService).Call(ctx, req.(*structpb.Struct))
	}
	return interceptor(ctx, request, info, handler)
}
