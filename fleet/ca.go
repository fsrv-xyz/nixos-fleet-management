package fleet

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"regexp"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"nixos-fleet-management/api"
)

const (
	agentCertValidity = 825 * 24 * time.Hour
	requestsPrefix    = "/ca/requests/"
	signedPrefix      = "/ca/signed/"
)

var certnamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

type CA struct {
	api.UnimplementedCertificateAuthorityServer
	cert *x509.Certificate
	key  crypto.Signer
	etcd *clientv3.Client
}

func LoadCA(certFile, keyFile string, etcd *clientv3.Client) (*CA, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &CA{cert: pair.Leaf, key: pair.PrivateKey.(crypto.Signer), etcd: etcd}, nil
}

func (c *CA) RequestCertificate(ctx context.Context, req *api.CertificateRequest) (*api.CertificateResponse, error) {
	csr, err := parseCSR(req.Csr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name := csr.Subject.CommonName
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr.Raw})

	txn, err := c.etcd.Txn(ctx).
		If(absent(signedPrefix+name), absent(requestsPrefix+name)).
		Then(clientv3.OpPut(requestsPrefix+name, string(csrPEM))).
		Else(clientv3.OpGet(signedPrefix+name), clientv3.OpGet(requestsPrefix+name)).
		Commit()
	if err != nil {
		return nil, err
	}
	if txn.Succeeded {
		log.Printf("certificate request from %s (%s)", name, Fingerprint(csr.Raw))
		return &api.CertificateResponse{}, nil
	}

	if kvs := txn.Responses[0].GetResponseRange().Kvs; len(kvs) > 0 {
		cert, err := parseCertificate(kvs[0].Value)
		if err != nil {
			return nil, err
		}
		if !samePublicKey(cert.PublicKey, csr.PublicKey) {
			return nil, status.Errorf(codes.AlreadyExists, "%s is signed for a different key", name)
		}
		return &api.CertificateResponse{Certificate: kvs[0].Value}, nil
	}
	pending, err := parseCSR(txn.Responses[1].GetResponseRange().Kvs[0].Value)
	if err != nil {
		return nil, err
	}
	if !samePublicKey(pending.PublicKey, csr.PublicKey) {
		return nil, status.Errorf(codes.AlreadyExists, "%s has a pending request for a different key", name)
	}
	return &api.CertificateResponse{}, nil
}

func (c *CA) List(ctx context.Context, w io.Writer) error {
	resp, err := c.etcd.Get(ctx, requestsPrefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	for _, kv := range resp.Kvs {
		csr, err := parseCSR(kv.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", kv.Key, err)
		}
		fmt.Fprintf(w, "%s %s\n", csr.Subject.CommonName, Fingerprint(csr.Raw))
	}
	return nil
}

func (c *CA) Sign(ctx context.Context, name string) error {
	resp, err := c.etcd.Get(ctx, requestsPrefix+name)
	if err != nil {
		return err
	}
	if len(resp.Kvs) == 0 {
		return fmt.Errorf("no pending request for %q", name)
	}
	csr, err := parseCSR(resp.Kvs[0].Value)
	if err != nil {
		return err
	}
	certPEM, err := c.issue(name, csr.PublicKey)
	if err != nil {
		return err
	}
	return c.commitIfUnchanged(ctx, requestsPrefix+name, resp.Kvs[0].ModRevision,
		clientv3.OpPut(signedPrefix+name, string(certPEM)), clientv3.OpDelete(requestsPrefix+name))
}

// Renew reissues the certificate on file for name, as long as it still belongs to pub.
func (c *CA) Renew(ctx context.Context, name string, pub crypto.PublicKey) ([]byte, error) {
	resp, err := c.etcd.Get(ctx, signedPrefix+name)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, status.Errorf(codes.PermissionDenied, "no certificate on file for %s", name)
	}
	cert, err := parseCertificate(resp.Kvs[0].Value)
	if err != nil {
		return nil, err
	}
	if !samePublicKey(cert.PublicKey, pub) {
		return nil, status.Errorf(codes.PermissionDenied, "certificate on file for %s has a different key", name)
	}
	certPEM, err := c.issue(name, pub)
	if err != nil {
		return nil, err
	}
	if err := c.commitIfUnchanged(ctx, signedPrefix+name, resp.Kvs[0].ModRevision, clientv3.OpPut(signedPrefix+name, string(certPEM))); err != nil {
		return nil, err
	}
	log.Printf("renewed certificate for %s", name)
	return certPEM, nil
}

func (c *CA) commitIfUnchanged(ctx context.Context, key string, modRevision int64, ops ...clientv3.Op) error {
	txn, err := c.etcd.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", modRevision)).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if !txn.Succeeded {
		return status.Errorf(codes.Aborted, "%s changed concurrently", key)
	}
	return nil
}

func absent(key string) clientv3.Cmp {
	return clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
}

func (c *CA) issue(name string, pub crypto.PublicKey) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now,
		NotAfter:     now.Add(agentCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, c.cert, pub, c.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + strings.ToUpper(fmt.Sprintf("%x", sum))
}

func parseCSR(data []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	if !certnamePattern.MatchString(csr.Subject.CommonName) {
		return nil, fmt.Errorf("invalid certname %q", csr.Subject.CommonName)
	}
	return csr, nil
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func samePublicKey(a, b crypto.PublicKey) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}
