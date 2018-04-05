// Copyright 2018 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tester

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/etcd/pkg/debugutil"
	"github.com/coreos/etcd/tools/functional-tester/rpcpb"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	yaml "gopkg.in/yaml.v2"
)

// Cluster defines tester cluster.
type Cluster struct {
	lg *zap.Logger

	agentConns    []*grpc.ClientConn
	agentClients  []rpcpb.TransportClient
	agentStreams  []rpcpb.Transport_TransportClient
	agentRequests []*rpcpb.Request

	testerHTTPServer *http.Server

	Members []*rpcpb.Member `yaml:"agent-configs"`
	Tester  *rpcpb.Tester   `yaml:"tester-config"`

	failures []Failure

	rateLimiter *rate.Limiter
	stresser    Stresser
	checker     Checker

	currentRevision int64
	rd              int
	cs              int
}

func newCluster(lg *zap.Logger, fpath string) (*Cluster, error) {
	bts, err := ioutil.ReadFile(fpath)
	if err != nil {
		return nil, err
	}
	lg.Info("opened configuration file", zap.String("path", fpath))

	clus := &Cluster{lg: lg}
	if err = yaml.Unmarshal(bts, clus); err != nil {
		return nil, err
	}

	for i := range clus.Members {
		if clus.Members[i].BaseDir == "" {
			return nil, fmt.Errorf("Members[i].BaseDir cannot be empty (got %q)", clus.Members[i].BaseDir)
		}
		if clus.Members[i].EtcdLogPath == "" {
			return nil, fmt.Errorf("Members[i].EtcdLogPath cannot be empty (got %q)", clus.Members[i].EtcdLogPath)
		}

		if clus.Members[i].Etcd.Name == "" {
			return nil, fmt.Errorf("'--name' cannot be empty (got %+v)", clus.Members[i])
		}
		if clus.Members[i].Etcd.DataDir == "" {
			return nil, fmt.Errorf("'--data-dir' cannot be empty (got %+v)", clus.Members[i])
		}
		if clus.Members[i].Etcd.SnapshotCount == 0 {
			return nil, fmt.Errorf("'--snapshot-count' cannot be 0 (got %+v)", clus.Members[i].Etcd.SnapshotCount)
		}
		if clus.Members[i].Etcd.DataDir == "" {
			return nil, fmt.Errorf("'--data-dir' cannot be empty (got %q)", clus.Members[i].Etcd.DataDir)
		}
		if clus.Members[i].Etcd.WALDir == "" {
			clus.Members[i].Etcd.WALDir = filepath.Join(clus.Members[i].Etcd.DataDir, "member", "wal")
		}

		if clus.Members[i].Etcd.HeartbeatIntervalMs == 0 {
			return nil, fmt.Errorf("'--heartbeat-interval' cannot be 0 (got %+v)", clus.Members[i].Etcd)
		}
		if clus.Members[i].Etcd.ElectionTimeoutMs == 0 {
			return nil, fmt.Errorf("'--election-timeout' cannot be 0 (got %+v)", clus.Members[i].Etcd)
		}
		if int64(clus.Tester.DelayLatencyMs) <= clus.Members[i].Etcd.ElectionTimeoutMs {
			return nil, fmt.Errorf("delay latency %d ms must be greater than election timeout %d ms", clus.Tester.DelayLatencyMs, clus.Members[i].Etcd.ElectionTimeoutMs)
		}

		port := ""
		listenClientPorts := make([]string, len(clus.Members))
		for i, u := range clus.Members[i].Etcd.ListenClientURLs {
			if !isValidURL(u) {
				return nil, fmt.Errorf("'--listen-client-urls' has valid URL %q", u)
			}
			listenClientPorts[i], err = getPort(u)
			if err != nil {
				return nil, fmt.Errorf("'--listen-client-urls' has no port %q", u)
			}
		}
		for i, u := range clus.Members[i].Etcd.AdvertiseClientURLs {
			if !isValidURL(u) {
				return nil, fmt.Errorf("'--advertise-client-urls' has valid URL %q", u)
			}
			port, err = getPort(u)
			if err != nil {
				return nil, fmt.Errorf("'--advertise-client-urls' has no port %q", u)
			}
			if clus.Members[i].EtcdClientProxy && listenClientPorts[i] == port {
				return nil, fmt.Errorf("clus.Members[%d] requires client port proxy, but advertise port %q conflicts with listener port %q", i, port, listenClientPorts[i])
			}
		}

		listenPeerPorts := make([]string, len(clus.Members))
		for i, u := range clus.Members[i].Etcd.ListenPeerURLs {
			if !isValidURL(u) {
				return nil, fmt.Errorf("'--listen-peer-urls' has valid URL %q", u)
			}
			listenPeerPorts[i], err = getPort(u)
			if err != nil {
				return nil, fmt.Errorf("'--listen-peer-urls' has no port %q", u)
			}
		}
		for i, u := range clus.Members[i].Etcd.InitialAdvertisePeerURLs {
			if !isValidURL(u) {
				return nil, fmt.Errorf("'--initial-advertise-peer-urls' has valid URL %q", u)
			}
			port, err = getPort(u)
			if err != nil {
				return nil, fmt.Errorf("'--initial-advertise-peer-urls' has no port %q", u)
			}
			if clus.Members[i].EtcdPeerProxy && listenPeerPorts[i] == port {
				return nil, fmt.Errorf("clus.Members[%d] requires peer port proxy, but advertise port %q conflicts with listener port %q", i, port, listenPeerPorts[i])
			}
		}

		if !strings.HasPrefix(clus.Members[i].EtcdLogPath, clus.Members[i].BaseDir) {
			return nil, fmt.Errorf("EtcdLogPath must be prefixed with BaseDir (got %q)", clus.Members[i].EtcdLogPath)
		}
		if !strings.HasPrefix(clus.Members[i].Etcd.DataDir, clus.Members[i].BaseDir) {
			return nil, fmt.Errorf("Etcd.DataDir must be prefixed with BaseDir (got %q)", clus.Members[i].Etcd.DataDir)
		}

		// TODO: support separate WALDir that can be handled via failure-archive
		if !strings.HasPrefix(clus.Members[i].Etcd.WALDir, clus.Members[i].BaseDir) {
			return nil, fmt.Errorf("Etcd.WALDir must be prefixed with BaseDir (got %q)", clus.Members[i].Etcd.WALDir)
		}

		if len(clus.Tester.FailureCases) == 0 {
			return nil, errors.New("FailureCases not found")
		}
	}

	if clus.Tester.DelayLatencyMs <= clus.Tester.DelayLatencyMsRv {
		return nil, fmt.Errorf("delay latency %d ms must be greater than delay latency random variable %d ms", clus.Tester.DelayLatencyMs, clus.Tester.DelayLatencyMsRv)
	}

	for _, v := range clus.Tester.FailureCases {
		if _, ok := rpcpb.FailureCase_value[v]; !ok {
			return nil, fmt.Errorf("%q is not defined in 'rpcpb.FailureCase_value'", v)
		}
	}

	for _, v := range clus.Tester.StressTypes {
		if _, ok := rpcpb.StressType_value[v]; !ok {
			return nil, fmt.Errorf("StressType is unknown; got %q", v)
		}
	}
	if clus.Tester.StressKeySuffixRangeTxn > 100 {
		return nil, fmt.Errorf("StressKeySuffixRangeTxn maximum value is 100, got %v", clus.Tester.StressKeySuffixRangeTxn)
	}
	if clus.Tester.StressKeyTxnOps > 64 {
		return nil, fmt.Errorf("StressKeyTxnOps maximum value is 64, got %v", clus.Tester.StressKeyTxnOps)
	}

	return clus, err
}

// TODO: status handler

var dialOpts = []grpc.DialOption{
	grpc.WithInsecure(),
	grpc.WithTimeout(5 * time.Second),
	grpc.WithBlock(),
}

// NewCluster creates a client from a tester configuration.
func NewCluster(lg *zap.Logger, fpath string) (*Cluster, error) {
	clus, err := newCluster(lg, fpath)
	if err != nil {
		return nil, err
	}

	clus.agentConns = make([]*grpc.ClientConn, len(clus.Members))
	clus.agentClients = make([]rpcpb.TransportClient, len(clus.Members))
	clus.agentStreams = make([]rpcpb.Transport_TransportClient, len(clus.Members))
	clus.agentRequests = make([]*rpcpb.Request, len(clus.Members))
	clus.failures = make([]Failure, 0)

	for i, ap := range clus.Members {
		var err error
		clus.agentConns[i], err = grpc.Dial(ap.AgentAddr, dialOpts...)
		if err != nil {
			return nil, err
		}
		clus.agentClients[i] = rpcpb.NewTransportClient(clus.agentConns[i])
		clus.lg.Info("connected", zap.String("agent-address", ap.AgentAddr))

		clus.agentStreams[i], err = clus.agentClients[i].Transport(context.Background())
		if err != nil {
			return nil, err
		}
		clus.lg.Info("created stream", zap.String("agent-address", ap.AgentAddr))
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	if clus.Tester.EnablePprof {
		for p, h := range debugutil.PProfHandlers() {
			mux.Handle(p, h)
		}
	}
	clus.testerHTTPServer = &http.Server{
		Addr:    clus.Tester.TesterAddr,
		Handler: mux,
	}
	go clus.serveTesterServer()

	clus.updateFailures()

	clus.rateLimiter = rate.NewLimiter(
		rate.Limit(int(clus.Tester.StressQPS)),
		int(clus.Tester.StressQPS),
	)

	clus.updateStresserChecker()

	return clus, nil
}

func (clus *Cluster) serveTesterServer() {
	clus.lg.Info(
		"started tester HTTP server",
		zap.String("tester-address", clus.Tester.TesterAddr),
	)
	err := clus.testerHTTPServer.ListenAndServe()
	clus.lg.Info(
		"tester HTTP server returned",
		zap.String("tester-address", clus.Tester.TesterAddr),
		zap.Error(err),
	)
	if err != nil && err != http.ErrServerClosed {
		clus.lg.Fatal("tester HTTP errored", zap.Error(err))
	}
}

func (clus *Cluster) updateFailures() {
	for _, cs := range clus.Tester.FailureCases {
		switch cs {
		case "KILL_ONE_FOLLOWER":
			clus.failures = append(clus.failures, newFailureKillOneFollower())
		case "KILL_ONE_FOLLOWER_UNTIL_TRIGGER_SNAPSHOT":
			clus.failures = append(clus.failures, newFailureKillOneFollowerUntilTriggerSnapshot())
		case "KILL_LEADER":
			clus.failures = append(clus.failures, newFailureKillLeader())
		case "KILL_LEADER_UNTIL_TRIGGER_SNAPSHOT":
			clus.failures = append(clus.failures, newFailureKillLeaderUntilTriggerSnapshot())
		case "KILL_QUORUM":
			clus.failures = append(clus.failures, newFailureKillQuorum())
		case "KILL_ALL":
			clus.failures = append(clus.failures, newFailureKillAll())

		case "BLACKHOLE_PEER_PORT_TX_RX_ONE_FOLLOWER":
			clus.failures = append(clus.failures, newFailureBlackholePeerPortTxRxOneFollower(clus))
		case "BLACKHOLE_PEER_PORT_TX_RX_ONE_FOLLOWER_UNTIL_TRIGGER_SNAPSHOT":
			clus.failures = append(clus.failures, newFailureBlackholePeerPortTxRxOneFollowerUntilTriggerSnapshot())
		case "BLACKHOLE_PEER_PORT_TX_RX_LEADER":
			clus.failures = append(clus.failures, newFailureBlackholePeerPortTxRxLeader(clus))
		case "BLACKHOLE_PEER_PORT_TX_RX_LEADER_UNTIL_TRIGGER_SNAPSHOT":
			clus.failures = append(clus.failures, newFailureBlackholePeerPortTxRxLeaderUntilTriggerSnapshot())
		case "BLACKHOLE_PEER_PORT_TX_RX_QUORUM":
			clus.failures = append(clus.failures, newFailureBlackholePeerPortTxRxQuorum(clus))
		case "BLACKHOLE_PEER_PORT_TX_RX_ALL":
			clus.failures = append(clus.failures, newFailureBlackholePeerPortTxRxAll(clus))

		case "DELAY_PEER_PORT_TX_RX_ONE_FOLLOWER":
			clus.failures = append(clus.failures, newFailureDelayPeerPortTxRxOneFollower(clus))
		case "DELAY_PEER_PORT_TX_RX_ONE_FOLLOWER_UNTIL_TRIGGER_SNAPSHOT":
			clus.failures = append(clus.failures, newFailureDelayPeerPortTxRxOneFollowerUntilTriggerSnapshot())
		case "DELAY_PEER_PORT_TX_RX_LEADER":
			clus.failures = append(clus.failures, newFailureDelayPeerPortTxRxLeader(clus))
		case "DELAY_PEER_PORT_TX_RX_LEADER_UNTIL_TRIGGER_SNAPSHOT":
			clus.failures = append(clus.failures, newFailureDelayPeerPortTxRxLeaderUntilTriggerSnapshot())
		case "DELAY_PEER_PORT_TX_RX_QUORUM":
			clus.failures = append(clus.failures, newFailureDelayPeerPortTxRxQuorum(clus))
		case "DELAY_PEER_PORT_TX_RX_ALL":
			clus.failures = append(clus.failures, newFailureDelayPeerPortTxRxAll(clus))

		case "NO_FAIL_WITH_STRESS":
			clus.failures = append(clus.failures, newFailureNoFailWithStress(clus))
		case "NO_FAIL_WITH_NO_STRESS_FOR_LIVENESS":
			clus.failures = append(clus.failures, newFailureNoFailWithNoStressForLiveness(clus))

		case "EXTERNAL":
			clus.failures = append(clus.failures, newFailureExternal(clus.Tester.ExternalExecPath))
		case "FAILPOINTS":
			fpFailures, fperr := failpointFailures(clus)
			if len(fpFailures) == 0 {
				clus.lg.Info("no failpoints found!", zap.Error(fperr))
			}
			clus.failures = append(clus.failures, fpFailures...)
		}
	}
}

func (clus *Cluster) failureStrings() (fs []string) {
	fs = make([]string, len(clus.failures))
	for i := range clus.failures {
		fs[i] = clus.failures[i].Desc()
	}
	return fs
}

func (clus *Cluster) shuffleFailures() {
	rand.Seed(time.Now().UnixNano())
	offset := rand.Intn(1000)
	n := len(clus.failures)
	cp := coprime(n)

	fs := make([]Failure, n)
	for i := 0; i < n; i++ {
		fs[i] = clus.failures[(cp*i+offset)%n]
	}
	clus.failures = fs
	clus.lg.Info("shuffled test failure cases", zap.Int("total", n))
}

/*
x and y of GCD 1 are coprime to each other

x1 = ( coprime of n * idx1 + offset ) % n
x2 = ( coprime of n * idx2 + offset ) % n
(x2 - x1) = coprime of n * (idx2 - idx1) % n
          = (idx2 - idx1) = 1

Consecutive x's are guaranteed to be distinct
*/
func coprime(n int) int {
	coprime := 1
	for i := n / 2; i < n; i++ {
		if gcd(i, n) == 1 {
			coprime = i
			break
		}
	}
	return coprime
}

func gcd(x, y int) int {
	if y == 0 {
		return x
	}
	return gcd(y, x%y)
}

func (clus *Cluster) updateStresserChecker() {
	cs := &compositeStresser{}
	for _, m := range clus.Members {
		cs.stressers = append(cs.stressers, newStresser(clus, m))
	}
	clus.stresser = cs

	if clus.Tester.ConsistencyCheck {
		clus.checker = newHashChecker(clus.lg, hashAndRevGetter(clus))
		if schk := cs.Checker(); schk != nil {
			clus.checker = newCompositeChecker([]Checker{clus.checker, schk})
		}
	} else {
		clus.checker = newNoChecker()
	}

	clus.lg.Info(
		"updated stressers",
		zap.Int("round", clus.rd),
		zap.Int("case", clus.cs),
	)
}

func (clus *Cluster) checkConsistency() (err error) {
	defer func() {
		if err != nil {
			return
		}
		if err = clus.updateRevision(); err != nil {
			clus.lg.Warn(
				"updateRevision failed",
				zap.Error(err),
			)
			return
		}
	}()

	if err = clus.checker.Check(); err != nil {
		clus.lg.Warn(
			"consistency check FAIL",
			zap.Int("round", clus.rd),
			zap.Int("case", clus.cs),
			zap.Error(err),
		)
		return err
	}
	clus.lg.Info(
		"consistency check ALL PASS",
		zap.Int("round", clus.rd),
		zap.Int("case", clus.cs),
		zap.String("desc", clus.failures[clus.cs].Desc()),
	)

	return err
}

// Bootstrap bootstraps etcd cluster the very first time.
// After this, just continue to call kill/restart.
func (clus *Cluster) Bootstrap() error {
	// this is the only time that creates request from scratch
	return clus.broadcastOperation(rpcpb.Operation_InitialStartEtcd)
}

// FailArchive sends "FailArchive" operation.
func (clus *Cluster) FailArchive() error {
	return clus.broadcastOperation(rpcpb.Operation_FailArchive)
}

// Restart sends "Restart" operation.
func (clus *Cluster) Restart() error {
	return clus.broadcastOperation(rpcpb.Operation_RestartEtcd)
}

func (clus *Cluster) broadcastOperation(op rpcpb.Operation) error {
	for i := range clus.agentStreams {
		err := clus.sendOperation(i, op)
		if err != nil {
			if op == rpcpb.Operation_DestroyEtcdAgent &&
				strings.Contains(err.Error(), "rpc error: code = Unavailable desc = transport is closing") {
				// agent server has already closed;
				// so this error is expected
				clus.lg.Info(
					"successfully destroyed",
					zap.String("member", clus.Members[i].EtcdClientEndpoint),
				)
				continue
			}
			return err
		}
	}
	return nil
}

func (clus *Cluster) sendOperation(idx int, op rpcpb.Operation) error {
	if op == rpcpb.Operation_InitialStartEtcd {
		clus.agentRequests[idx] = &rpcpb.Request{
			Operation: op,
			Member:    clus.Members[idx],
			Tester:    clus.Tester,
		}
	} else {
		clus.agentRequests[idx].Operation = op
	}

	err := clus.agentStreams[idx].Send(clus.agentRequests[idx])
	clus.lg.Info(
		"sent request",
		zap.String("operation", op.String()),
		zap.String("to", clus.Members[idx].EtcdClientEndpoint),
		zap.Error(err),
	)
	if err != nil {
		return err
	}

	resp, err := clus.agentStreams[idx].Recv()
	if resp != nil {
		clus.lg.Info(
			"received response",
			zap.String("operation", op.String()),
			zap.String("from", clus.Members[idx].EtcdClientEndpoint),
			zap.Bool("success", resp.Success),
			zap.String("status", resp.Status),
			zap.Error(err),
		)
	} else {
		clus.lg.Info(
			"received empty response",
			zap.String("operation", op.String()),
			zap.String("from", clus.Members[idx].EtcdClientEndpoint),
			zap.Error(err),
		)
	}
	if err != nil {
		return err
	}

	if !resp.Success {
		err = errors.New(resp.Status)
	}
	return err
}

// DestroyEtcdAgents terminates all tester connections to agents and etcd servers.
func (clus *Cluster) DestroyEtcdAgents() {
	err := clus.broadcastOperation(rpcpb.Operation_DestroyEtcdAgent)
	if err != nil {
		clus.lg.Warn("destroying etcd/agents FAIL", zap.Error(err))
	} else {
		clus.lg.Info("destroying etcd/agents PASS")
	}

	for i, conn := range clus.agentConns {
		err := conn.Close()
		clus.lg.Info("closed connection to agent", zap.String("agent-address", clus.Members[i].AgentAddr), zap.Error(err))
	}

	if clus.testerHTTPServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := clus.testerHTTPServer.Shutdown(ctx)
		cancel()
		clus.lg.Info("closed tester HTTP server", zap.String("tester-address", clus.Tester.TesterAddr), zap.Error(err))
	}
}

// WaitHealth ensures all members are healthy
// by writing a test key to etcd cluster.
func (clus *Cluster) WaitHealth() error {
	var err error
	// wait 60s to check cluster health.
	// TODO: set it to a reasonable value. It is set that high because
	// follower may use long time to catch up the leader when reboot under
	// reasonable workload (https://github.com/coreos/etcd/issues/2698)
	for i := 0; i < 60; i++ {
		for _, m := range clus.Members {
			if err = m.WriteHealthKey(); err != nil {
				clus.lg.Warn(
					"health check FAIL",
					zap.Int("retries", i),
					zap.String("endpoint", m.EtcdClientEndpoint),
					zap.Error(err),
				)
				break
			}
			clus.lg.Info(
				"health check PASS",
				zap.Int("retries", i),
				zap.String("endpoint", m.EtcdClientEndpoint),
			)
		}
		if err == nil {
			clus.lg.Info(
				"health check ALL PASS",
				zap.Int("round", clus.rd),
				zap.Int("case", clus.cs),
			)
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

// GetLeader returns the index of leader and error if any.
func (clus *Cluster) GetLeader() (int, error) {
	for i, m := range clus.Members {
		isLeader, err := m.IsLeader()
		if isLeader || err != nil {
			return i, err
		}
	}
	return 0, fmt.Errorf("no leader found")
}

// maxRev returns the maximum revision found on the cluster.
func (clus *Cluster) maxRev() (rev int64, err error) {
	ctx, cancel := context.WithTimeout(context.TODO(), time.Second)
	defer cancel()
	revc, errc := make(chan int64, len(clus.Members)), make(chan error, len(clus.Members))
	for i := range clus.Members {
		go func(m *rpcpb.Member) {
			mrev, merr := m.Rev(ctx)
			revc <- mrev
			errc <- merr
		}(clus.Members[i])
	}
	for i := 0; i < len(clus.Members); i++ {
		if merr := <-errc; merr != nil {
			err = merr
		}
		if mrev := <-revc; mrev > rev {
			rev = mrev
		}
	}
	return rev, err
}

func (clus *Cluster) getRevisionHash() (map[string]int64, map[string]int64, error) {
	revs := make(map[string]int64)
	hashes := make(map[string]int64)
	for _, m := range clus.Members {
		rev, hash, err := m.RevHash()
		if err != nil {
			return nil, nil, err
		}
		revs[m.EtcdClientEndpoint] = rev
		hashes[m.EtcdClientEndpoint] = hash
	}
	return revs, hashes, nil
}

func (clus *Cluster) compactKV(rev int64, timeout time.Duration) (err error) {
	if rev <= 0 {
		return nil
	}

	for i, m := range clus.Members {
		clus.lg.Info(
			"compact START",
			zap.String("endpoint", m.EtcdClientEndpoint),
			zap.Int64("compact-revision", rev),
			zap.Duration("timeout", timeout),
		)
		now := time.Now()
		cerr := m.Compact(rev, timeout)
		succeed := true
		if cerr != nil {
			if strings.Contains(cerr.Error(), "required revision has been compacted") && i > 0 {
				clus.lg.Info(
					"compact error is ignored",
					zap.String("endpoint", m.EtcdClientEndpoint),
					zap.Int64("compact-revision", rev),
					zap.Error(cerr),
				)
			} else {
				clus.lg.Warn(
					"compact FAIL",
					zap.String("endpoint", m.EtcdClientEndpoint),
					zap.Int64("compact-revision", rev),
					zap.Error(cerr),
				)
				err = cerr
				succeed = false
			}
		}

		if succeed {
			clus.lg.Info(
				"compact PASS",
				zap.String("endpoint", m.EtcdClientEndpoint),
				zap.Int64("compact-revision", rev),
				zap.Duration("timeout", timeout),
				zap.Duration("took", time.Since(now)),
			)
		}
	}
	return err
}

func (clus *Cluster) checkCompact(rev int64) error {
	if rev == 0 {
		return nil
	}
	for _, m := range clus.Members {
		if err := m.CheckCompact(rev); err != nil {
			return err
		}
	}
	return nil
}

func (clus *Cluster) defrag() error {
	for _, m := range clus.Members {
		if err := m.Defrag(); err != nil {
			clus.lg.Warn(
				"defrag FAIL",
				zap.String("endpoint", m.EtcdClientEndpoint),
				zap.Error(err),
			)
			return err
		}
		clus.lg.Info(
			"defrag PASS",
			zap.String("endpoint", m.EtcdClientEndpoint),
		)
	}
	clus.lg.Info(
		"defrag ALL PASS",
		zap.Int("round", clus.rd),
		zap.Int("case", clus.cs),
	)
	return nil
}

// GetFailureDelayDuration computes failure delay duration.
func (clus *Cluster) GetFailureDelayDuration() time.Duration {
	return time.Duration(clus.Tester.FailureDelayMs) * time.Millisecond
}

// Report reports the number of modified keys.
func (clus *Cluster) Report() int64 {
	return clus.stresser.ModifiedKeys()
}
