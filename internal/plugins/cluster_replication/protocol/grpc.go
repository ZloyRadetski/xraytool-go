package protocol

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const serviceName = "xraytool.replication.v1.Replication"

type ReplicationClient interface {
	Connect(ctx context.Context, opts ...grpc.CallOption) (Replication_ConnectClient, error)
}

type replicationClient struct{ cc grpc.ClientConnInterface }

func NewReplicationClient(cc grpc.ClientConnInterface) ReplicationClient {
	return &replicationClient{cc: cc}
}

func (c *replicationClient) Connect(ctx context.Context, opts ...grpc.CallOption) (Replication_ConnectClient, error) {
	stream, err := c.cc.NewStream(ctx, &Replication_ServiceDesc.Streams[0], "/"+serviceName+"/Connect", opts...)
	if err != nil {
		return nil, err
	}
	return &replicationConnectClient{ClientStream: stream}, nil
}

type Replication_ConnectClient interface {
	Send(*wrapperspb.BytesValue) error
	Recv() (*wrapperspb.BytesValue, error)
	grpc.ClientStream
}

type replicationConnectClient struct{ grpc.ClientStream }

func (x *replicationConnectClient) Send(message *wrapperspb.BytesValue) error {
	return x.ClientStream.SendMsg(message)
}
func (x *replicationConnectClient) Recv() (*wrapperspb.BytesValue, error) {
	message := new(wrapperspb.BytesValue)
	if err := x.ClientStream.RecvMsg(message); err != nil {
		return nil, err
	}
	return message, nil
}

type ReplicationServer interface {
	Connect(Replication_ConnectServer) error
}

type UnimplementedReplicationServer struct{}

func (UnimplementedReplicationServer) Connect(Replication_ConnectServer) error { return nil }

type Replication_ConnectServer interface {
	Send(*wrapperspb.BytesValue) error
	Recv() (*wrapperspb.BytesValue, error)
	grpc.ServerStream
}

type replicationConnectServer struct{ grpc.ServerStream }

func (x *replicationConnectServer) Send(message *wrapperspb.BytesValue) error {
	return x.ServerStream.SendMsg(message)
}
func (x *replicationConnectServer) Recv() (*wrapperspb.BytesValue, error) {
	message := new(wrapperspb.BytesValue)
	if err := x.ServerStream.RecvMsg(message); err != nil {
		return nil, err
	}
	return message, nil
}

func RegisterReplicationServer(server grpc.ServiceRegistrar, implementation ReplicationServer) {
	server.RegisterService(&Replication_ServiceDesc, implementation)
}

func replicationConnectHandler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(ReplicationServer).Connect(&replicationConnectServer{ServerStream: stream})
}

var Replication_ServiceDesc = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*ReplicationServer)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName:    "Connect",
		Handler:       replicationConnectHandler,
		ServerStreams: true,
		ClientStreams: true,
	}},
}
