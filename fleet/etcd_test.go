package fleet

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

func TestEtcdClientNamespace(t *testing.T) {
	raw := startEtcd(t)
	etcd, err := NewEtcdClient(raw.Endpoints(), "/fleet/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { etcd.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	watch := etcd.Watch(ctx, agentsPrefix, clientv3.WithPrefix())
	path := storePath('a', "nixos-system-node1")
	if _, err := SetDesired(ctx, etcd, "node1", path); err != nil {
		t.Fatal(err)
	}

	resp, err := raw.Get(ctx, "/fleet/agents/node1/desired")
	if err != nil || len(resp.Kvs) != 1 || string(resp.Kvs[0].Value) != path {
		t.Fatalf("raw key below prefix: %v, %v", resp, err)
	}
	select {
	case wr := <-watch:
		if key := string(wr.Events[0].Kv.Key); key != "/agents/node1/desired" {
			t.Errorf("watched key %q, want it without prefix", key)
		}
	case <-ctx.Done():
		t.Fatal("no watch event")
	}
	session, err := concurrency.NewSession(etcd)
	if err != nil {
		t.Fatal(err)
	}
	session.Close()
}
