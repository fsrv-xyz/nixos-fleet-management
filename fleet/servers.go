package fleet

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const srvPrefix = "srv:"

// ResolveServers turns a single "srv:<name>" into the targets of the SRV records
// of name and returns any other list unchanged.
func ResolveServers(ctx context.Context, r *net.Resolver, servers []string) ([]string, error) {
	if len(servers) != 1 || !strings.HasPrefix(servers[0], srvPrefix) {
		return servers, nil
	}
	name := strings.TrimPrefix(servers[0], srvPrefix)
	_, records, err := r.LookupSRV(ctx, "", "", name)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, rec := range records {
		// RFC 2782: a target of "." means the service is decidedly not available.
		if host := strings.TrimSuffix(rec.Target, "."); host != "" {
			addrs = append(addrs, net.JoinHostPort(host, strconv.Itoa(int(rec.Port))))
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no usable SRV records for %s", name)
	}
	return addrs, nil
}
