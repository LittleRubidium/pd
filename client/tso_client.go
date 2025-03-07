// Copyright 2023 TiKV Project Authors.
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

package pd

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/tsopb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/client/errs"
	"github.com/tikv/pd/client/tsoutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TSOClient is the client used to get timestamps.
type TSOClient interface {
	// GetTS gets a timestamp from PD or TSO microservice.
	GetTS(ctx context.Context) (int64, int64, error)
	// GetTSAsync gets a timestamp from PD or TSO microservice, without block the caller.
	GetTSAsync(ctx context.Context) TSFuture
	// GetLocalTS gets a local timestamp from PD or TSO microservice.
	GetLocalTS(ctx context.Context, dcLocation string) (int64, int64, error)
	// GetLocalTSAsync gets a local timestamp from PD or TSO microservice, without block the caller.
	GetLocalTSAsync(ctx context.Context, dcLocation string) TSFuture
	// GetMinTS gets a timestamp from PD or the minimal timestamp across all keyspace groups from
	// the TSO microservice.
	GetMinTS(ctx context.Context) (int64, int64, error)
}

type tsoRequest struct {
	start      time.Time
	clientCtx  context.Context
	requestCtx context.Context
	done       chan error
	physical   int64
	logical    int64
	dcLocation string
}

var tsoReqPool = sync.Pool{
	New: func() interface{} {
		return &tsoRequest{
			done:     make(chan error, 1),
			physical: 0,
			logical:  0,
		}
	},
}

type tsoClient struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	option *option

	keyspaceID   uint32
	svcDiscovery ServiceDiscovery
	tsoStreamBuilderFactory
	// tsoAllocators defines the mapping {dc-location -> TSO allocator leader URL}
	tsoAllocators sync.Map // Store as map[string]string
	// tsoAllocServingAddrSwitchedCallback will be called when any global/local
	// tso allocator leader is switched.
	tsoAllocServingAddrSwitchedCallback []func()

	// tsoDispatcher is used to dispatch different TSO requests to
	// the corresponding dc-location TSO channel.
	tsoDispatcher sync.Map // Same as map[string]chan *tsoRequest
	// dc-location -> deadline
	tsDeadline sync.Map // Same as map[string]chan deadline
	// dc-location -> *lastTSO
	lastTSMap sync.Map // Same as map[string]*lastTSO

	checkTSDeadlineCh         chan struct{}
	checkTSODispatcherCh      chan struct{}
	updateTSOConnectionCtxsCh chan struct{}
}

// newTSOClient returns a new TSO client.
func newTSOClient(
	ctx context.Context, option *option, keyspaceID uint32,
	svcDiscovery ServiceDiscovery, factory tsoStreamBuilderFactory,
) *tsoClient {
	ctx, cancel := context.WithCancel(ctx)
	c := &tsoClient{
		ctx:                       ctx,
		cancel:                    cancel,
		option:                    option,
		keyspaceID:                keyspaceID,
		svcDiscovery:              svcDiscovery,
		tsoStreamBuilderFactory:   factory,
		checkTSDeadlineCh:         make(chan struct{}),
		checkTSODispatcherCh:      make(chan struct{}, 1),
		updateTSOConnectionCtxsCh: make(chan struct{}, 1),
	}

	eventSrc := svcDiscovery.(tsoAllocatorEventSource)
	eventSrc.SetTSOLocalServAddrsUpdatedCallback(c.updateTSOLocalServAddrs)
	eventSrc.SetTSOGlobalServAddrUpdatedCallback(c.updateTSOGlobalServAddr)
	c.svcDiscovery.AddServiceAddrsSwitchedCallback(c.scheduleUpdateTSOConnectionCtxs)

	return c
}

func (c *tsoClient) Setup() {
	c.svcDiscovery.CheckMemberChanged()
	c.updateTSODispatcher()

	// Start the daemons.
	c.wg.Add(2)
	go c.tsoDispatcherCheckLoop()
	go c.tsCancelLoop()
}

// Close closes the TSO client
func (c *tsoClient) Close() {
	if c == nil {
		return
	}
	log.Info("closing tso client")

	c.cancel()
	c.wg.Wait()

	log.Info("close tso client")
	c.tsoDispatcher.Range(func(_, dispatcherInterface interface{}) bool {
		if dispatcherInterface != nil {
			dispatcher := dispatcherInterface.(*tsoDispatcher)
			tsoErr := errors.WithStack(errClosing)
			dispatcher.tsoBatchController.revokePendingRequest(tsoErr)
			dispatcher.dispatcherCancel()
		}
		return true
	})

	log.Info("tso client is closed")
}

// GetTSOAllocators returns {dc-location -> TSO allocator leader URL} connection map
func (c *tsoClient) GetTSOAllocators() *sync.Map {
	return &c.tsoAllocators
}

// GetTSOAllocatorServingAddrByDCLocation returns the tso allocator of the given dcLocation
func (c *tsoClient) GetTSOAllocatorServingAddrByDCLocation(dcLocation string) (string, bool) {
	url, exist := c.tsoAllocators.Load(dcLocation)
	if !exist {
		return "", false
	}
	return url.(string), true
}

// GetTSOAllocatorClientConnByDCLocation returns the tso allocator grpc client connection
// of the given dcLocation
func (c *tsoClient) GetTSOAllocatorClientConnByDCLocation(dcLocation string) (*grpc.ClientConn, string) {
	url, ok := c.tsoAllocators.Load(dcLocation)
	if !ok {
		panic(fmt.Sprintf("the allocator leader in %s should exist", dcLocation))
	}
	cc, ok := c.svcDiscovery.GetClientConns().Load(url)
	if !ok {
		panic(fmt.Sprintf("the client connection of %s in %s should exist", url, dcLocation))
	}
	return cc.(*grpc.ClientConn), url.(string)
}

// AddTSOAllocatorServingAddrSwitchedCallback adds callbacks which will be called
// when any global/local tso allocator service endpoint is switched.
func (c *tsoClient) AddTSOAllocatorServingAddrSwitchedCallback(callbacks ...func()) {
	c.tsoAllocServingAddrSwitchedCallback = append(c.tsoAllocServingAddrSwitchedCallback, callbacks...)
}

func (c *tsoClient) updateTSOLocalServAddrs(allocatorMap map[string]string) error {
	if len(allocatorMap) == 0 {
		return nil
	}

	updated := false

	// Switch to the new one
	for dcLocation, addr := range allocatorMap {
		if len(addr) == 0 {
			continue
		}
		oldAddr, exist := c.GetTSOAllocatorServingAddrByDCLocation(dcLocation)
		if exist && addr == oldAddr {
			continue
		}
		updated = true
		if _, err := c.svcDiscovery.GetOrCreateGRPCConn(addr); err != nil {
			log.Warn("[tso] failed to connect dc tso allocator serving address",
				zap.String("dc-location", dcLocation),
				zap.String("serving-address", addr),
				errs.ZapError(err))
			return err
		}
		c.tsoAllocators.Store(dcLocation, addr)
		log.Info("[tso] switch dc tso allocator serving address",
			zap.String("dc-location", dcLocation),
			zap.String("new-address", addr),
			zap.String("old-address", oldAddr))
	}

	// Garbage collection of the old TSO allocator primaries
	c.gcAllocatorServingAddr(allocatorMap)

	if updated {
		c.scheduleCheckTSODispatcher()
	}

	return nil
}

func (c *tsoClient) updateTSOGlobalServAddr(addr string) error {
	c.tsoAllocators.Store(globalDCLocation, addr)
	log.Info("[tso] switch dc tso allocator serving address",
		zap.String("dc-location", globalDCLocation),
		zap.String("new-address", addr))
	c.scheduleCheckTSODispatcher()
	return nil
}

func (c *tsoClient) gcAllocatorServingAddr(curAllocatorMap map[string]string) {
	// Clean up the old TSO allocators
	c.tsoAllocators.Range(func(dcLocationKey, _ interface{}) bool {
		dcLocation := dcLocationKey.(string)
		// Skip the Global TSO Allocator
		if dcLocation == globalDCLocation {
			return true
		}
		if _, exist := curAllocatorMap[dcLocation]; !exist {
			log.Info("[tso] delete unused tso allocator", zap.String("dc-location", dcLocation))
			c.tsoAllocators.Delete(dcLocation)
		}
		return true
	})
}

// backupClientConn gets a grpc client connection of the current reachable and healthy
// backup service endpoints randomly. Backup service endpoints are followers in a
// quorum-based cluster or secondaries in a primary/secondary configured cluster.
func (c *tsoClient) backupClientConn() (*grpc.ClientConn, string) {
	addrs := c.svcDiscovery.GetBackupAddrs()
	if len(addrs) < 1 {
		return nil, ""
	}
	var (
		cc  *grpc.ClientConn
		err error
	)
	for i := 0; i < len(addrs); i++ {
		addr := addrs[rand.Intn(len(addrs))]
		if cc, err = c.svcDiscovery.GetOrCreateGRPCConn(addr); err != nil {
			continue
		}
		healthCtx, healthCancel := context.WithTimeout(c.ctx, c.option.timeout)
		resp, err := healthpb.NewHealthClient(cc).Check(healthCtx, &healthpb.HealthCheckRequest{Service: ""})
		healthCancel()
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return cc, addr
		}
	}
	return nil, ""
}

// getMinTS gets a timestamp from PD or the minimal timestamp across all keyspace groups from the TSO microservice.
func (c *tsoClient) getMinTS(ctx context.Context) (physical, logical int64, err error) {
	// Immediately refresh the TSO server/pod list
	addrs, err := c.svcDiscovery.DiscoverMicroservice(tsoService)
	if err != nil {
		return 0, 0, errs.ErrClientGetMinTSO.Wrap(err).GenWithStackByCause()
	}
	if len(addrs) == 0 {
		return 0, 0, errs.ErrClientGetMinTSO.FastGenByArgs("no tso servers/pods discovered")
	}

	// Get the minimal timestamp from the TSO servers/pods
	var mutex sync.Mutex
	resps := make([]*tsopb.GetMinTSResponse, 0)
	wg := sync.WaitGroup{}
	wg.Add(len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			defer wg.Done()
			resp, err := c.getMinTSFromSingleServer(ctx, addr, c.option.timeout)
			if err != nil || resp == nil {
				log.Warn("[tso] failed to get min ts from tso server",
					zap.String("address", addr), zap.Error(err))
				return
			}
			mutex.Lock()
			defer mutex.Unlock()
			resps = append(resps, resp)
		}(addr)
	}
	wg.Wait()

	// Check the results. The returned minimal timestamp is valid if all the conditions are met:
	// 1. The number of responses is equal to the number of TSO servers/pods.
	// 2. The number of keyspace groups asked is equal to the number of TSO servers/pods.
	// 3. The minimal timestamp is not zero.
	var (
		minTS               *pdpb.Timestamp
		keyspaceGroupsAsked uint32
	)
	if len(resps) == 0 {
		return 0, 0, errs.ErrClientGetMinTSO.FastGenByArgs("none of tso server/pod responded")
	}
	emptyTS := &pdpb.Timestamp{}
	keyspaceGroupsTotal := resps[0].KeyspaceGroupsTotal
	for _, resp := range resps {
		if resp.KeyspaceGroupsTotal == 0 {
			return 0, 0, errs.ErrClientGetMinTSO.FastGenByArgs("the tso service has no keyspace group")
		}
		if resp.KeyspaceGroupsTotal != keyspaceGroupsTotal {
			return 0, 0, errs.ErrClientGetMinTSO.FastGenByArgs(
				"the tso service has inconsistent keyspace group total count")
		}
		keyspaceGroupsAsked += resp.KeyspaceGroupsServing
		if tsoutil.CompareTimestamp(resp.Timestamp, emptyTS) > 0 &&
			(minTS == nil || tsoutil.CompareTimestamp(resp.Timestamp, minTS) < 0) {
			minTS = resp.Timestamp
		}
	}

	if keyspaceGroupsAsked != keyspaceGroupsTotal {
		return 0, 0, errs.ErrClientGetMinTSO.FastGenByArgs(
			fmt.Sprintf("can't query all the tso keyspace groups. Asked %d, expected %d",
				keyspaceGroupsAsked, keyspaceGroupsTotal))
	}

	if minTS == nil {
		return 0, 0, errs.ErrClientGetMinTSO.FastGenByArgs("the tso service is not ready")
	}

	return minTS.Physical, tsoutil.AddLogical(minTS.Logical, 0, minTS.SuffixBits), nil
}

func (c *tsoClient) getMinTSFromSingleServer(
	ctx context.Context, tsoSrvAddr string, timeout time.Duration,
) (*tsopb.GetMinTSResponse, error) {
	cc, err := c.svcDiscovery.GetOrCreateGRPCConn(tsoSrvAddr)
	if err != nil {
		return nil, errs.ErrClientGetMinTSO.FastGenByArgs(
			fmt.Sprintf("can't connect to tso server %s", tsoSrvAddr))
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := tsopb.NewTSOClient(cc).GetMinTS(
		cctx, &tsopb.GetMinTSRequest{
			Header: &tsopb.RequestHeader{
				ClusterId:       c.svcDiscovery.GetClusterID(),
				KeyspaceId:      c.svcDiscovery.GetKeyspaceID(),
				KeyspaceGroupId: c.svcDiscovery.GetKeyspaceGroupID(),
			},
			DcLocation: globalDCLocation,
		})
	if err != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s",
			err, cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMinTSO.Wrap(attachErr).GenWithStackByCause()
	}
	if resp == nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s",
			"no min ts info collected", cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMinTSO.Wrap(attachErr).GenWithStackByCause()
	}
	if resp.GetHeader().GetError() != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s",
			resp.GetHeader().GetError().String(), cc.Target(), cc.GetState().String())
		return nil, errs.ErrClientGetMinTSO.Wrap(attachErr).GenWithStackByCause()
	}

	return resp, nil
}
