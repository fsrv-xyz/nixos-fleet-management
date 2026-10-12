package fleet

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"

	"nixos-fleet-management/api"
)

// Per agent: <id>/facts, <id>/desired, <id>/deploy and, under the lease of the
// connected server, <id>/conns/<lease>-<n>.
const (
	agentsPrefix       = "/agents/"
	maxFactsSize       = 64 << 10
	maxProgressMessage = 1 << 10
)

type Server struct {
	api.UnimplementedFleetServer
	ca       *CA
	etcd     *clientv3.Client
	name     string
	lease    clientv3.LeaseID
	conns    atomic.Uint64
	feed     agentFeed
	mu       sync.Mutex
	sessions map[string]map[*agentSession]struct{}
}

// agentSession receives the desired store path of a connected agent.
type agentSession struct {
	desired latest[string]
}

// agentFeed holds the latest agent list as JSON; changed is closed and replaced
// on every update, so waiters skip intermediate lists they were too slow for.
type agentFeed struct {
	mu      sync.Mutex
	agents  []byte
	changed chan struct{}
}

func (f *agentFeed) set(agents []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = agents
	close(f.changed)
	f.changed = make(chan struct{})
}

// get returns the latest agent list, nil before the first sync, and a channel
// closed on the next update.
func (f *agentFeed) get() ([]byte, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents, f.changed
}

type AgentStatus struct {
	ID          string          `json:"id"`
	Server      string          `json:"server"`
	Address     string          `json:"address"`
	ConnectedAt time.Time       `json:"connectedAt"`
	Facts       json.RawMessage `json:"facts,omitempty"`
	Desired     string          `json:"desired,omitempty"`
	Deploy      json.RawMessage `json:"deploy,omitempty"`
}

type DeployStatus struct {
	StorePath string    `json:"storePath"`
	Phase     string    `json:"phase"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NewServer registers connected agents in etcd under lease, so they disappear
// with the server when it stops renewing the lease.
func NewServer(ca *CA, etcd *clientv3.Client, name string, lease clientv3.LeaseID) *Server {
	return &Server{
		ca:       ca,
		etcd:     etcd,
		name:     name,
		lease:    lease,
		feed:     agentFeed{changed: make(chan struct{})},
		sessions: map[string]map[*agentSession]struct{}{},
	}
}

func NewGRPCServer(creds credentials.TransportCredentials, srv *Server) *grpc.Server {
	s := grpc.NewServer(
		grpc.Creds(creds),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	api.RegisterFleetServer(s, srv)
	return s
}

func (s *Server) Connect(stream grpc.BidiStreamingServer[api.AgentMessage, api.ServerMessage]) error {
	id, err := AgentID(stream.Context())
	if err != nil {
		return err
	}
	ctx := stream.Context()
	p, _ := peer.FromContext(ctx)
	agent := AgentStatus{ID: id, Server: s.name, Address: p.Addr.String(), ConnectedAt: time.Now()}
	key := fmt.Sprintf("%s%s/conns/%x-%d", agentsPrefix, id, s.lease, s.conns.Add(1))
	if err := s.putAgent(ctx, key, agent); err != nil {
		return status.Errorf(codes.Unavailable, "register agent: %v", err)
	}
	log.Printf("agent %s connected from %s", id, agent.Address)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.etcd.Delete(ctx, key); err != nil {
			log.Printf("unregister agent %s: %v", id, err)
		}
		log.Printf("agent %s disconnected", id)
	}()

	session := &agentSession{desired: make(latest[string], 1)}
	s.addSession(id, session)
	defer s.removeSession(id, session)
	resp, err := s.etcd.Get(ctx, agentsPrefix+id+"/desired")
	if err != nil {
		return status.Errorf(codes.Unavailable, "read desired state: %v", err)
	}
	if len(resp.Kvs) > 0 {
		session.desired.offer(string(resp.Kvs[0].Value))
	}

	// Recv runs apart, so this goroutine is the only one calling Send.
	msgs := make(chan *api.AgentMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case msgs <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case storePath := <-session.desired:
			if err := stream.Send(&api.ServerMessage{Body: &api.ServerMessage_Deploy{Deploy: &api.Deploy{StorePath: storePath}}}); err != nil {
				return err
			}
		case msg := <-msgs:
			if err := s.handle(ctx, stream, id, msg); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, stream grpc.BidiStreamingServer[api.AgentMessage, api.ServerMessage], id string, msg *api.AgentMessage) error {
	switch body := msg.Body.(type) {
	case *api.AgentMessage_Heartbeat:
		return stream.Send(&api.ServerMessage{Body: &api.ServerMessage_Ack{Ack: body.Heartbeat.Seq}})
	case *api.AgentMessage_Facts:
		return s.storeFacts(ctx, id, body.Facts)
	case *api.AgentMessage_DeployProgress:
		return s.storeDeployProgress(ctx, id, body.DeployProgress)
	default:
		return status.Error(codes.InvalidArgument, "empty agent message")
	}
}

func (s *Server) addSession(id string, session *agentSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[id] == nil {
		s.sessions[id] = map[*agentSession]struct{}{}
	}
	s.sessions[id][session] = struct{}{}
}

func (s *Server) removeSession(id string, session *agentSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions[id], session)
	if len(s.sessions[id]) == 0 {
		delete(s.sessions, id)
	}
}

func (s *Server) offerDesired(id, storePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for session := range s.sessions[id] {
		session.desired.offer(storePath)
	}
}

// storeDeployProgress keeps only the latest progress per agent, without lease, so
// a failed deployment stays visible after the agent disconnected.
func (s *Server) storeDeployProgress(ctx context.Context, id string, progress *api.DeployProgress) error {
	if !storePathPattern.MatchString(progress.StorePath) {
		return status.Errorf(codes.InvalidArgument, "invalid store path %q", progress.StorePath)
	}
	phase, ok := api.DeployProgress_Phase_name[int32(progress.Phase)]
	if !ok || progress.Phase == api.DeployProgress_PHASE_UNSPECIFIED {
		return status.Errorf(codes.InvalidArgument, "invalid phase %d", progress.Phase)
	}
	data, err := json.Marshal(DeployStatus{
		StorePath: progress.StorePath,
		Phase:     phase,
		Message:   truncate(progress.Message, maxProgressMessage),
		UpdatedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	if _, err := s.etcd.Put(ctx, agentsPrefix+id+"/deploy", string(data)); err != nil {
		return status.Errorf(codes.Unavailable, "store deploy progress: %v", err)
	}
	return nil
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return strings.ToValidUTF8(text[:max], "")
}

// storeFacts writes facts only if they differ from the stored ones, so reconnects
// with unchanged facts cause no revision and no watch event.
func (s *Server) storeFacts(ctx context.Context, id string, facts *api.Facts) error {
	if size := proto.Size(facts); size > maxFactsSize {
		return status.Errorf(codes.InvalidArgument, "facts of %d bytes exceed %d", size, maxFactsSize)
	}
	for _, fact := range facts.Facts {
		if _, err := protoregistry.GlobalTypes.FindMessageByURL(fact.TypeUrl); err != nil {
			return status.Errorf(codes.InvalidArgument, "unknown fact type %q", fact.TypeUrl)
		}
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(facts)
	if err != nil {
		return err
	}
	key := agentsPrefix + id + "/facts"
	_, err = s.etcd.Txn(ctx).
		If(clientv3.Compare(clientv3.Value(key), "=", string(data))).
		Else(clientv3.OpPut(key, string(data))).
		Commit()
	if err != nil {
		return status.Errorf(codes.Unavailable, "store facts: %v", err)
	}
	return nil
}

func (s *Server) putAgent(ctx context.Context, key string, agent AgentStatus) error {
	data, err := json.Marshal(agent)
	if err != nil {
		return err
	}
	_, err = s.etcd.Put(ctx, key, string(data), clientv3.WithLease(s.lease))
	return err
}

// WatchAgents follows all agents in etcd through a single watch until ctx ends:
// it keeps the web frontend's agent list current and passes desired state
// changes to the agents connected to this server.
func (s *Server) WatchAgents(ctx context.Context) {
	for {
		err := s.watchAgents(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("agent watch: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// watchAgents publishes the open agent streams of all servers with their agent's
// facts, first the current list and then after every change. An agent appears
// twice while its old stream is not yet detected as dead.
func (s *Server) watchAgents(ctx context.Context) error {
	resp, err := s.etcd.Get(ctx, agentsPrefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	agents := map[string]*agentState{}
	for _, kv := range resp.Kvs {
		if err := applyAgentKey(agents, kv.Key, kv.Value); err != nil {
			return err
		}
	}
	// Agents connected here may have missed desired changes while the watch was down.
	for id, state := range agents {
		if state.desired != "" {
			s.offerDesired(id, state.desired)
		}
	}
	if err := s.publish(agents); err != nil {
		return err
	}

	watch := s.etcd.Watch(clientv3.WithRequireLeader(ctx), agentsPrefix, clientv3.WithPrefix(), clientv3.WithRev(resp.Header.Revision+1))
	for wr := range watch {
		if err := wr.Err(); err != nil {
			return err
		}
		for _, ev := range wr.Events {
			var value []byte
			if ev.Type != clientv3.EventTypeDelete {
				value = ev.Kv.Value
			}
			if err := applyAgentKey(agents, ev.Kv.Key, value); err != nil {
				return err
			}
			if id, sub := splitAgentKey(ev.Kv.Key); sub == "desired" && value != nil {
				s.offerDesired(id, string(value))
			}
		}
		if err := s.publish(agents); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// WithRequireLeader closes the channel on leader loss without ending ctx.
	return errors.New("agent watch closed")
}

type agentState struct {
	facts   json.RawMessage
	desired string
	deploy  json.RawMessage
	conns   map[string]AgentStatus
}

func splitAgentKey(key []byte) (id, sub string) {
	id, sub, _ = strings.Cut(strings.TrimPrefix(string(key), agentsPrefix), "/")
	return id, sub
}

// applyAgentKey updates agents with a key below agentsPrefix; a nil value deletes it.
func applyAgentKey(agents map[string]*agentState, key, value []byte) error {
	id, sub := splitAgentKey(key)
	state := agents[id]
	if state == nil {
		state = &agentState{conns: map[string]AgentStatus{}}
		agents[id] = state
	}
	switch {
	case sub == "facts":
		state.facts = nil
		if value != nil {
			state.facts = factsJSON(key, value)
		}
	case sub == "desired":
		state.desired = string(value)
	case sub == "deploy":
		state.deploy = value
	case strings.HasPrefix(sub, "conns/"):
		if value == nil {
			delete(state.conns, sub)
			break
		}
		var conn AgentStatus
		if err := json.Unmarshal(value, &conn); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		state.conns[sub] = conn
	}
	if state.facts == nil && state.desired == "" && state.deploy == nil && len(state.conns) == 0 {
		delete(agents, id)
	}
	return nil
}

// factsJSON logs and skips facts it cannot decode, such as a fact type this
// server version does not know, instead of stopping the whole feed.
func factsJSON(key, value []byte) json.RawMessage {
	var facts api.Facts
	if err := proto.Unmarshal(value, &facts); err != nil {
		log.Printf("%s: %v", key, err)
		return nil
	}
	data, err := protojson.Marshal(&facts)
	if err != nil {
		log.Printf("%s: %v", key, err)
		return nil
	}
	return data
}

func (s *Server) publish(agents map[string]*agentState) error {
	var list []AgentStatus
	for _, state := range agents {
		for _, conn := range state.conns {
			conn.Facts, conn.Desired, conn.Deploy = state.facts, state.desired, state.deploy
			list = append(list, conn)
		}
	}
	data, err := json.Marshal(sortedAgents(list))
	if err != nil {
		return err
	}
	s.feed.set(data)
	return nil
}

// sortedAgents never returns nil, which would be published as null instead of [].
func sortedAgents(list []AgentStatus) []AgentStatus {
	if list == nil {
		list = []AgentStatus{}
	}
	slices.SortFunc(list, func(a, b AgentStatus) int {
		return cmp.Or(strings.Compare(a.ID, b.ID), a.ConnectedAt.Compare(b.ConnectedAt))
	})
	return list
}

func (s *Server) RenewCertificate(ctx context.Context, _ *api.RenewCertificateRequest) (*api.CertificateResponse, error) {
	cert, err := clientCertificate(ctx)
	if err != nil {
		return nil, err
	}
	certPEM, err := s.ca.Renew(ctx, cert.Subject.CommonName, cert.PublicKey)
	if err != nil {
		return nil, err
	}
	return &api.CertificateResponse{Certificate: certPEM}, nil
}

func AgentID(ctx context.Context) (string, error) {
	cert, err := clientCertificate(ctx)
	if err != nil {
		return "", err
	}
	return cert.Subject.CommonName, nil
}

func clientCertificate(ctx context.Context) (*x509.Certificate, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no verified client certificate")
	}
	return info.State.VerifiedChains[0][0], nil
}
