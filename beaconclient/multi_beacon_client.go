// Package beaconclient provides a beacon-node client
package beaconclient

import (
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/mev-boost-relay/common"
	"github.com/sirupsen/logrus"
	uberatomic "go.uber.org/atomic"
)

var (
	ErrBeaconNodeSyncing        = errors.New("beacon node is syncing or unavailable")
	ErrBeaconNodesUnavailable   = errors.New("all beacon nodes responded with error")
	ErrWithdrawalsBeforeCapella = errors.New("withdrawals are not supported before capella")
)

// IMultiBeaconClient is the interface for the MultiBeaconClient, which can manage several beacon client instances under the hood
type IMultiBeaconClient interface {
	BestSyncStatus() (*SyncStatusPayloadData, error)
	SubscribeToHeadEvents(slotC chan HeadEventData)

	// FetchValidators returns all active and pending validators from the beacon node
	FetchValidators(headSlot uint64) (map[types.PubkeyHex]ValidatorResponseEntry, error)
	GetProposerDuties(epoch uint64) (*ProposerDutiesResponse, error)
	PublishBlock(block *common.SignedBeaconBlock) (code int, err error)
	GetGenesis() (*GetGenesisResponse, error)
	GetSpec() (spec *GetSpecResponse, err error)
	GetForkSchedule() (spec *GetForkScheduleResponse, err error)
	GetBlock(blockID string) (block *GetBlockResponse, err error)
	GetRandao(slot uint64) (spec *GetRandaoResponse, err error)
	GetWithdrawals(slot uint64) (spec *GetWithdrawalsResponse, err error)
}

// IBeaconInstance is the interface for a single beacon client instance
type IBeaconInstance interface {
	SyncStatus() (*SyncStatusPayloadData, error)
	CurrentSlot() (uint64, error)
	SubscribeToHeadEvents(slotC chan HeadEventData)
	FetchValidators(headSlot uint64) (map[types.PubkeyHex]ValidatorResponseEntry, error)
	GetProposerDuties(epoch uint64) (*ProposerDutiesResponse, error)
	GetURI() string
	PublishBlock(block *common.SignedBeaconBlock) (code int, err error)
	GetGenesis() (*GetGenesisResponse, error)
	GetSpec() (spec *GetSpecResponse, err error)
	GetForkSchedule() (spec *GetForkScheduleResponse, err error)
	GetBlock(blockID string) (*GetBlockResponse, error)
	GetRandao(slot uint64) (spec *GetRandaoResponse, err error)
	GetWithdrawals(slot uint64) (spec *GetWithdrawalsResponse, err error)
}

type MultiBeaconClient struct {
	log             *logrus.Entry
	bestBeaconIndex uberatomic.Int64
	beaconInstances []IBeaconInstance

	// feature flags
	ffAllowSyncingBeaconNode bool
}

func NewMultiBeaconClient(log *logrus.Entry, beaconInstances []IBeaconInstance) *MultiBeaconClient {
	client := &MultiBeaconClient{
		log:                      log.WithField("component", "beaconClient"),
		beaconInstances:          beaconInstances,
		bestBeaconIndex:          *uberatomic.NewInt64(0),
		ffAllowSyncingBeaconNode: false,
	}

	// feature flags
	if os.Getenv("ALLOW_SYNCING_BEACON_NODE") != "" {
		client.log.Warn("env: ALLOW_SYNCING_BEACON_NODE: allow syncing beacon node")
		client.ffAllowSyncingBeaconNode = true
	}

	return client
}

func (c *MultiBeaconClient) BestSyncStatus() (*SyncStatusPayloadData, error) {
	var bestSyncStatus *SyncStatusPayloadData
	var foundSyncedNode bool

	// Check each beacon-node sync status
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, instance := range c.beaconInstances {
		wg.Add(1)
		go func(instance IBeaconInstance) {
			defer wg.Done()
			log := c.log.WithField("uri", instance.GetURI())
			log.Debug("getting sync status")

			syncStatus, err := instance.SyncStatus()
			if err != nil {
				log.WithError(err).Error("failed to get sync status")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			if foundSyncedNode {
				return
			}

			if bestSyncStatus == nil {
				bestSyncStatus = syncStatus
			}

			if !syncStatus.IsSyncing {
				bestSyncStatus = syncStatus
				foundSyncedNode = true
			}
		}(instance)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if !foundSyncedNode && !c.ffAllowSyncingBeaconNode {
		return nil, ErrBeaconNodeSyncing
	}

	if bestSyncStatus == nil {
		return nil, ErrBeaconNodesUnavailable
	}

	return bestSyncStatus, nil
}

// SubscribeToHeadEvents subscribes to head events from all beacon nodes. A single head event will be received multiple times,
// likely once for every beacon nodes.
func (c *MultiBeaconClient) SubscribeToHeadEvents(slotC chan HeadEventData) {
	for _, instance := range c.beaconInstances {
		go instance.SubscribeToHeadEvents(slotC)
	}
}

func (c *MultiBeaconClient) FetchValidators(headSlot uint64) (map[types.PubkeyHex]ValidatorResponseEntry, error) {
	// return the first successful beacon node response
	clients := c.beaconInstancesByLastResponse()

	for i, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		log.Debug("fetching validators")

		validators, err := client.FetchValidators(headSlot)
		if err != nil {
			log.WithError(err).Error("failed to fetch validators")
			continue
		}

		c.bestBeaconIndex.Store(int64(i))

		// Received successful response. Set this index as last successful beacon node
		return validators, nil
	}

	return nil, ErrBeaconNodesUnavailable
}

func (c *MultiBeaconClient) GetProposerDuties(epoch uint64) (*ProposerDutiesResponse, error) {
	// return the first successful beacon node response
	clients := c.beaconInstancesByLastResponse()
	log := c.log.WithField("epoch", epoch)

	for i, client := range clients {
		log := log.WithField("uri", client.GetURI())
		log.Debug("fetching proposer duties")

		duties, err := client.GetProposerDuties(epoch)
		if err != nil {
			log.WithError(err).Error("failed to get proposer duties")
			continue
		}

		c.bestBeaconIndex.Store(int64(i))

		// Received successful response. Set this index as last successful beacon node
		return duties, nil
	}

	return nil, ErrBeaconNodesUnavailable
}

// beaconInstancesByLastResponse returns a list of beacon clients that has the client
// with the last successful response as the first element of the slice
func (c *MultiBeaconClient) beaconInstancesByLastResponse() []IBeaconInstance {
	index := c.bestBeaconIndex.Load()
	if index == 0 {
		return c.beaconInstances
	}

	instances := make([]IBeaconInstance, len(c.beaconInstances))
	copy(instances, c.beaconInstances)
	instances[0], instances[index] = instances[index], instances[0]

	return instances
}

// PublishBlock publishes the signed beacon block via https://ethereum.github.io/beacon-APIs/#/ValidatorRequiredApi/publishBlock
func (c *MultiBeaconClient) PublishBlock(block *common.SignedBeaconBlock) (code int, err error) {
	log := c.log.WithFields(logrus.Fields{
		"slot":      block.Slot(),
		"blockHash": block.BlockHash(),
	})

	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := log.WithField("uri", client.GetURI())
		log.Debug("publishing block")

		if code, err = client.PublishBlock(block); err != nil {
			log.WithField("statusCode", code).WithError(err).Warn("failed to publish block")
			continue
		}

		log.WithField("statusCode", code).Info("published block")
		return code, nil
	}

	log.WithField("statusCode", code).WithError(err).Error("failed to publish block on any CL node")
	return code, err
}

// GetGenesis returns the genesis info - https://ethereum.github.io/beacon-APIs/#/Beacon/getGenesis
func (c *MultiBeaconClient) GetGenesis() (genesisInfo *GetGenesisResponse, err error) {
	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		if genesisInfo, err = client.GetGenesis(); err != nil {
			log.WithError(err).Warn("failed to get genesis info")
			continue
		}

		return genesisInfo, nil
	}

	c.log.WithError(err).Error("failed to get genesis info on any CL node")
	return nil, err
}

// GetSpec - https://ethereum.github.io/beacon-APIs/#/Config/getSpec
func (c *MultiBeaconClient) GetSpec() (spec *GetSpecResponse, err error) {
	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		if spec, err = client.GetSpec(); err != nil {
			log.WithError(err).Warn("failed to get spec")
			continue
		}

		return spec, nil
	}

	c.log.WithError(err).Error("failed to get spec on any CL node")
	return nil, err
}

// GetForkSchedule - https://ethereum.github.io/beacon-APIs/#/Config/getForkSchedule
func (c *MultiBeaconClient) GetForkSchedule() (spec *GetForkScheduleResponse, err error) {
	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		if spec, err = client.GetForkSchedule(); err != nil {
			log.WithError(err).Warn("failed to get fork schedule")
			continue
		}

		return spec, nil
	}

	c.log.WithError(err).Error("failed to get fork schedule on any CL node")
	return nil, err
}

// GetBlock returns a block - https://ethereum.github.io/beacon-APIs/#/Beacon/getBlockV2
func (c *MultiBeaconClient) GetBlock(blockID string) (block *GetBlockResponse, err error) {
	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		if block, err = client.GetBlock(blockID); err != nil {
			log.WithField("blockID", blockID).WithError(err).Warn("failed to get block")
			continue
		}

		return block, nil
	}

	c.log.WithField("blockID", blockID).WithError(err).Error("failed to get block from any CL node")
	return nil, err
}

// GetRandao - 3500/eth/v1/beacon/states/<slot>/randao
func (c *MultiBeaconClient) GetRandao(slot uint64) (randaoResp *GetRandaoResponse, err error) {
	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		if randaoResp, err = client.GetRandao(slot); err != nil {
			log.WithField("slot", slot).WithError(err).Warn("failed to get randao")
			continue
		}

		return randaoResp, nil
	}

	c.log.WithField("slot", slot).WithError(err).Warn("failed to get randao from any CL node")
	return nil, err
}

// GetWithdrawals - 3500/eth/v1/beacon/states/<slot>/withdrawals
func (c *MultiBeaconClient) GetWithdrawals(slot uint64) (withdrawalsResp *GetWithdrawalsResponse, err error) {
	clients := c.beaconInstancesByLastResponse()
	for _, client := range clients {
		log := c.log.WithField("uri", client.GetURI())
		if withdrawalsResp, err = client.GetWithdrawals(slot); err != nil {
			if strings.Contains(err.Error(), "Withdrawals not enabled before capella") {
				break
			}
			log.WithField("slot", slot).WithError(err).Warn("failed to get withdrawals")
			continue
		}

		return withdrawalsResp, nil
	}

	if strings.Contains(err.Error(), "Withdrawals not enabled before capella") {
		c.log.WithField("slot", slot).WithError(err).Debug("failed to get withdrawals as capella has not been reached")
		return nil, ErrWithdrawalsBeforeCapella
	}

	c.log.WithField("slot", slot).WithError(err).Warn("failed to get withdrawals from any CL node")
	return nil, err
}
