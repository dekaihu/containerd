package disk

import (
	"context"
	"fmt"
	"time"

	pb "github.com/containerd/containerd/api/services/disks/v1"
	"github.com/containerd/containerd/integration/remote/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
)

var (
	timeout = 1 * time.Minute
)

type DisksClient struct {
	conn   *grpc.ClientConn
	client pb.DisksClient
}

func NewDisksClient(socketPath string) (*DisksClient, error) {

	klog.V(3).Infof("Connecting to disks service %s", socketPath)
	addr, dialer, err := util.GetAddressAndDialer(socketPath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, fmt.Errorf("connect gRPC failed: %w", err)
	}

	return &DisksClient{
		conn:   conn,
		client: pb.NewDisksClient(conn),
	}, nil
}

func (c *DisksClient) Close() error {
	return c.conn.Close()
}

func (c *DisksClient) View(ctx context.Context, key string) (*pb.ViewDiskResponse, error) {
	return c.client.View(ctx, &pb.ViewDiskRequest{
		Key: key,
	})
}

func (c *DisksClient) List(ctx context.Context, filter string, pageSize int32) (*pb.ListDiskResponse, error) {
	return c.client.List(ctx, &pb.ListDiskRequest{
		Filter:   filter,
		PageSize: pageSize,
	})
}
