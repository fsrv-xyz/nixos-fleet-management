package fleet

import (
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
)

// NewEtcdClient returns a client whose keys, watches and leases all live below
// prefix, so several fleets can share one etcd cluster.
func NewEtcdClient(endpoints []string, prefix string) (*clientv3.Client, error) {
	client, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimSuffix(prefix, "/")
	client.KV = namespace.NewKV(client.KV, prefix)
	client.Watcher = namespace.NewWatcher(client.Watcher, prefix)
	client.Lease = namespace.NewLease(client.Lease, prefix)
	return client, nil
}
