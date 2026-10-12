package fleet

import (
	"context"
	"net"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestResolveServers(t *testing.T) {
	r := fakeSRVResolver(t, map[string][]dnsmessage.SRVResource{
		"_fleet._tcp.example.test.": {
			{Priority: 10, Weight: 10, Port: 8443, Target: dnsmessage.MustNewName("fleet1.example.test.")},
			{Priority: 20, Weight: 10, Port: 9443, Target: dnsmessage.MustNewName("fleet2.example.test.")},
		},
		"_none._tcp.example.test.": {
			{Port: 0, Target: dnsmessage.MustNewName(".")},
		},
	})
	ctx := context.Background()

	addrs, err := ResolveServers(ctx, r, []string{"srv:_fleet._tcp.example.test."})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(addrs)
	if want := []string{"fleet1.example.test:8443", "fleet2.example.test:9443"}; !slices.Equal(addrs, want) {
		t.Errorf("srv: %v, want %v", addrs, want)
	}

	if _, err := ResolveServers(ctx, r, []string{"srv:_none._tcp.example.test."}); err == nil {
		t.Error("service marked unavailable resolved without error")
	}

	addrs, err = ResolveServers(ctx, r, []string{"a:8443", "b:8443"})
	if err != nil || !slices.Equal(addrs, []string{"a:8443", "b:8443"}) {
		t.Errorf("list: %v, %v", addrs, err)
	}
}

func fakeSRVResolver(t *testing.T, records map[string][]dnsmessage.SRVResource) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			b.StartQuestions()
			b.Question(q)
			b.StartAnswers()
			if q.Type == dnsmessage.TypeSRV {
				for _, srv := range records[q.Name.String()] {
					b.SRVResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, srv)
				}
			}
			msg, err := b.Finish()
			if err != nil {
				continue
			}
			pc.WriteTo(msg, addr)
		}
	}()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", pc.LocalAddr().String())
		},
	}
}
