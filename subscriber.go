package subscriber

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	ethclients "github.com/Iwinswap/iwinswap-ethclients"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	defaultNewBlockBuffer          = 100
	defaultBlockByNumberTimeout    = 10 * time.Second
	defaultUpdateClientSetInterval = 1 * time.Minute
)

type BlockSubscriber struct {
	getHealthyClients       func() []ethclients.ETHClient
	blockByNumberTimeout    time.Duration
	updateClientSetInterval time.Duration
	newBlockC               chan *types.Block
	newBlockReceiverC       chan *types.Block
	latestBlock             *types.Block
	clientSet               map[ethclients.ETHClient]struct{}
	cancelFunc              func()
	closed                  atomic.Bool
	wg                      sync.WaitGroup
	mu                      sync.RWMutex
	logger                  Logger
}

type SubscriberConfig struct {
	UpdateClientSetInterval time.Duration
	BlockByNumberTimeout    time.Duration
	NewBlockBuffer          int
	Logger                  Logger
}

func (config *SubscriberConfig) applyDefaults() {
	if config.UpdateClientSetInterval == 0 {
		config.UpdateClientSetInterval = defaultUpdateClientSetInterval
	}

	if config.BlockByNumberTimeout == 0 {
		config.BlockByNumberTimeout = defaultBlockByNumberTimeout
	}

	if config.NewBlockBuffer == 0 {
		config.NewBlockBuffer = defaultNewBlockBuffer
	}

	if config.Logger == nil {
		config.Logger = &DefaultLogger{}
	}
}

func NewBlockSubscriber(
	parentContext context.Context,
	newBlockReceiverC chan *types.Block,
	getHealthyClients func() []ethclients.ETHClient,
	config *SubscriberConfig,
) *BlockSubscriber {
	if config == nil {
		config = &SubscriberConfig{}
	}

	config.applyDefaults()
	ctx, cancel := context.WithCancel(parentContext)
	bs := &BlockSubscriber{
		clientSet:               make(map[ethclients.ETHClient]struct{}),
		newBlockC:               make(chan *types.Block, config.NewBlockBuffer),
		newBlockReceiverC:       newBlockReceiverC,
		updateClientSetInterval: config.UpdateClientSetInterval,
		blockByNumberTimeout:    config.BlockByNumberTimeout,
		getHealthyClients:       getHealthyClients,
		cancelFunc:              cancel,
		logger:                  config.Logger,
	}

	bs.wg.Add(2)
	go bs.updateClientSet(ctx)
	go bs.monitorNewBlocks(ctx)

	return bs
}

func (bs *BlockSubscriber) updateClientSet(ctx context.Context) {
	defer bs.wg.Done()

	ticker := time.NewTicker(bs.updateClientSetInterval)
	defer ticker.Stop()

	bs.discoverAndSubscribe(ctx) // Run once immediately on startup

	for {
		select {
		case <-ticker.C:
			bs.discoverAndSubscribe(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (bs *BlockSubscriber) discoverAndSubscribe(ctx context.Context) {
	clients := bs.getHealthyClients()
	for _, client := range clients {
		if !bs.isKnownClient(client) {
			bs.addToClientSet(client)
			bs.wg.Add(1)
			go bs.subscribeNewBlocks(
				ctx,
				client,
				bs.deleteFromClientSet,
			)
		}
	}
}

// MonitorNewBlocks listens for new blocks and updates the latest block.
func (bs *BlockSubscriber) monitorNewBlocks(ctx context.Context) {
	defer bs.wg.Done()

	for {
		select {
		case <-ctx.Done():
			bs.logger.Info("[BlockSubscriber]: MonitorNewBlocks shutting down")
			return

		case b := <-bs.newBlockC:
			bs.processNewBlock(ctx, b)
		}
	}
}

// logic:
// on subscription error, exit!
func (bs *BlockSubscriber) subscribeNewBlocks(
	ctx context.Context, // Manager's lifecycle context
	client ethclients.ETHClient,
	deleteKnownClient func(ethclients.ETHClient),
) {
	defer bs.wg.Done()
	defer deleteKnownClient(client)

	clientIdentifier := fmt.Sprintf("client-%p", client) // Basic identifier
	bs.logger.Debug(fmt.Sprintf("[BlockSubscriber]: Attempting to subscribe to new heads for %s", clientIdentifier))

	headers := make(chan *types.Header)
	sub, err := client.SubscribeNewHead(ctx, headers)
	if err != nil {
		bs.logger.Error(fmt.Sprintf("[BlockSubscriber]: Failed to subscribe to new heads for %s: %v", clientIdentifier, err))
		return
	}

	bs.logger.Debug(fmt.Sprintf("[BlockSubscriber]: Successfully subscribed to new heads for %s", clientIdentifier))
	defer sub.Unsubscribe()

	for {
		select {
		case header := <-headers:
			if header == nil {
				bs.logger.Warn(fmt.Sprintf("[BlockSubscriber]: Received nil header from %s, skipping.", clientIdentifier))
				continue
			}

			bs.logger.Debug(fmt.Sprintf("[BlockSubscriber]: Received new header from %s: Number %d, Hash %s", clientIdentifier, header.Number, header.Hash().Hex()))

			opCtx, opCancel := context.WithTimeout(ctx, bs.blockByNumberTimeout)
			block, err := client.BlockByNumber(opCtx, header.Number)
			opCancel()

			if err != nil {
				// no need to termintate if this fails
				bs.logger.Warn(fmt.Sprintf("[BlockSubscriber]: Failed to get block details by number from %s (Block %d, Hash %s): %v",
					clientIdentifier, header.Number, header.Hash().Hex(), err))

				continue
			}

			select {
			case bs.newBlockC <- block:
			case <-ctx.Done():
				bs.logger.Debug(fmt.Sprintf("[BlockSubscriber]: Subscription goroutine for %s shutting down (manager context done while sending to newBlockC): %v", clientIdentifier, ctx.Err()))
				return
			}

		case err := <-sub.Err():
			bs.logger.Error(fmt.Sprintf("[BlockSubscriber]: Subscription error received from %s: %v", clientIdentifier, err))
			return

		case <-ctx.Done():
			bs.logger.Debug(fmt.Sprintf("[BlockSubscriber]: context for %s cancelled (likely manager shutdown): %v", clientIdentifier, ctx.Err()))
			return // Exit the goroutine as the manager context is likely done.
		}
	}
}

// Problem:
// We blindly assume the client with the longest height is the best
// but we don't take into account the fact that the clients reorg and some of them haven't gotten the newest fork
// Decision:
// i think this is acceptable
// BlockSubscriber gives no guarantees that it handles reorgs
// It only guarantees correct subscription and prompt delivery of new blocks
func (bs *BlockSubscriber) processNewBlock(
	ctx context.Context,
	newBlock *types.Block) {
	if newBlock == nil {
		bs.logger.Warn("[BlockSubscriber]: received nil block")
		return
	}

	prevBlock := bs.LatestBlock()

	if prevBlock == nil || newBlock.NumberU64() > prevBlock.NumberU64() {
		// set latest known block
		bs.setLatestBlock(newBlock)

		if bs.newBlockReceiverC != nil {
			select {
			case bs.newBlockReceiverC <- newBlock:
			case <-ctx.Done():
			default:
				bs.logger.Warn(fmt.Sprintf("[BlockSubscriber]: newBlockReceiverC full — dropping block %d", newBlock.NumberU64()))
			}
		}
	}
}

func (bs *BlockSubscriber) addToClientSet(client ethclients.ETHClient) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.clientSet[client] = struct{}{}
}

func (bs *BlockSubscriber) isKnownClient(client ethclients.ETHClient) bool {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	_, known := bs.clientSet[client]
	return known
}

func (bs *BlockSubscriber) deleteFromClientSet(client ethclients.ETHClient) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	delete(bs.clientSet, client)
}

func (bs *BlockSubscriber) setLatestBlock(b *types.Block) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.latestBlock = b
}

func (bs *BlockSubscriber) LatestBlock() *types.Block {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	return bs.latestBlock
}

func (bs *BlockSubscriber) Close() {
	if bs.closed.CompareAndSwap(false, true) {
		bs.cancelFunc()
		bs.wg.Wait()
		// clients aren't ours so we don't close them!
	}
}
