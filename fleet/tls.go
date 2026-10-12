package fleet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/credentials"
)

const serverCertValidity = 7 * 24 * time.Hour

// ServerCertificate is this server's own certificate, issued by the CA at start
// and replaced in memory before it expires.
type ServerCertificate struct {
	ca       *CA
	names    []string
	validity time.Duration
	current  atomic.Pointer[tls.Certificate]
}

func NewServerCertificate(ca *CA, names []string) (*ServerCertificate, error) {
	return newServerCertificate(ca, names, serverCertValidity)
}

func newServerCertificate(ca *CA, names []string, validity time.Duration) (*ServerCertificate, error) {
	s := &ServerCertificate{ca: ca, names: names, validity: validity}
	cert, err := ca.issueServerCertificate(names, validity)
	if err != nil {
		return nil, err
	}
	s.current.Store(cert)
	return s, nil
}

// Run reissues the certificate after two thirds of its validity until ctx ends.
func (s *ServerCertificate) Run(ctx context.Context) {
	for {
		renewAt := s.current.Load().Leaf.NotAfter.Add(-s.validity / 3)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(renewAt)):
		}
		cert, err := s.ca.issueServerCertificate(s.names, s.validity)
		if err != nil {
			log.Printf("server certificate: %v", err)
			continue
		}
		s.current.Store(cert)
		log.Printf("server certificate renewed, valid until %s", cert.Leaf.NotAfter)
	}
}

func (s *ServerCertificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.current.Load(), nil
}

// ServerCredentials accept only agents with a client certificate from ca.
func ServerCredentials(ca *CA, cert *ServerCertificate) credentials.TransportCredentials {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return credentials.NewTLS(&tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: cert.get,
		ClientCAs:      pool,
		ClientAuth:     tls.RequireAndVerifyClientCert,
	})
}

// CAServerCredentials serve agents that have no certificate yet.
func CAServerCredentials(cert *ServerCertificate) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: cert.get,
	})
}

func bootstrapCredentials(caFile string) (credentials.TransportCredentials, error) {
	pool, err := loadPool(caFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
	}), nil
}

func loadPool(caFile string) (*x509.CertPool, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %s", caFile)
	}
	return pool, nil
}
