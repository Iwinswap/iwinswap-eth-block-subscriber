package subscriber

import (
	"context"
	"errors"
	"math/big"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ethclients "github.com/Iwinswap/iwinswap-ethclients"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
)

// NoOpLogger is a logger that performs no action, useful for silencing logs in tests.
type NoOpLogger struct{}

func (l *NoOpLogger) Debug(msg string, args ...any) {}
func (l *NoOpLogger) Info(msg string, args ...any)  {}
func (l *NoOpLogger) Warn(msg string, args ...any)  {}
func (l *NoOpLogger) Error(msg string, args ...any) {}

// testEnv holds all the components needed for a single resilience test scenario.
type testEnv struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	ethClients            []ethclients.ETHClient
	subscriptionErrorChs  map[ethclients.ETHClient]chan error
	getHealthyClientsFunc func() []ethclients.ETHClient
}

// setupResilienceTest creates a full mock environment for testing the BlockSubscriber.
// It sets up a mock block producer and a specified number of mock ETH clients.
func setupResilienceTest(t *testing.T, numclients int) *testEnv {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	currentBlockNumber := uint64(100)
	currentBlock := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(int64(currentBlockNumber))})
	blockInterval := 10 * time.Millisecond

	// Goroutine to simulate a blockchain producing new blocks.
	go func() {
		ticker := time.NewTicker(blockInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				currentBlockNumber++
				currentBlock = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(int64(currentBlockNumber))})
				mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Goroutine to simulate a client's head subscription feed.
	setUpNewHeadSubscription := func(ch chan<- *types.Header, stop <-chan struct{}) {
		ticker := time.NewTicker(blockInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				header := currentBlock.Header()
				mu.Unlock()
				select {
				case ch <- header:
				default:
				}
			case <-stop:
				return
			}
		}
	}

	var ethClients []ethclients.ETHClient
	subscriptionErrorChs := make(map[ethclients.ETHClient]chan error)

	for i := 0; i < numclients; i++ {
		client := ethclients.NewTestETHClient()
		errCh := make(chan error, 1)
		subscriptionErrorChs[client] = errCh

		client.SetBlockByNumberHandler(func(ctx context.Context, number *big.Int) (*types.Block, error) {
			mu.Lock()
			block := currentBlock
			mu.Unlock()
			return block, nil
		})

		client.SetSubscribeNewHeadHandler(func(ctx context.Context, ch chan<- *types.Header) (ethereum.Subscription, error) {
			stopCh := make(chan struct{})
			unsubscribe := func() { close(stopCh) }
			errFunc := func() <-chan error { return errCh }
			go setUpNewHeadSubscription(ch, stopCh)
			return ethclients.NewTestSubscription(unsubscribe, errFunc), nil
		})
		ethClients = append(ethClients, client)
	}

	return &testEnv{
		ctx:                  ctx,
		cancel:               cancel,
		ethClients:           ethClients,
		subscriptionErrorChs: subscriptionErrorChs,
		getHealthyClientsFunc: func() []ethclients.ETHClient {
			return ethClients // Simple provider that always returns the full list.
		},
	}
}

func TestNewBlockSubscriber(t *testing.T) {
	t.Run("Should init and successfully close a BlockSubscriber", func(t *testing.T) {
		blockSubscriber := NewBlockSubscriber(
			context.Background(),
			make(chan *types.Block),
			func() []ethclients.ETHClient { return nil },
			&SubscriberConfig{Logger: &NoOpLogger{}},
		)
		blockSubscriber.Close()
	})
}

func TestBlockSubscriberResilience(t *testing.T) {
	t.Run("should process blocks from multiple clients and close cleanly", func(t *testing.T) {
		env := setupResilienceTest(t, 200)
		defer env.cancel()

		var wg sync.WaitGroup
		wg.Add(1)
		numBlocksReceived := atomic.Uint32{}
		newBlockReceiver := make(chan *types.Block, 1000)
		const stopAfterNBlocks = 500

		go func() {
			defer wg.Done()
			for {
				select {
				case <-newBlockReceiver:
					if numBlocksReceived.Add(1) >= stopAfterNBlocks {
						return
					}
				case <-env.ctx.Done():
					return
				}
			}
		}()

		blockSubscriber := NewBlockSubscriber(
			env.ctx,
			newBlockReceiver,
			env.getHealthyClientsFunc,
			&SubscriberConfig{Logger: &NoOpLogger{}},
		)

		wg.Wait()
		blockSubscriber.Close()

		assert.NotNil(t, blockSubscriber.LatestBlock())
		assert.Equal(t, uint32(stopAfterNBlocks), numBlocksReceived.Load())
	})

	t.Run("should drop failed clients when refresh is disabled", func(t *testing.T) {
		env := setupResilienceTest(t, 200)
		defer env.cancel()

		// Chaos goroutine to inject permanent subscription failures.
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			rand.Seed(time.Now().UnixNano())
			for {
				select {
				case <-ticker.C:
					clientIndex := rand.Intn(len(env.ethClients))
					clientToFail := env.ethClients[clientIndex]
					if errCh := env.subscriptionErrorChs[clientToFail]; errCh != nil {
						select {
						case errCh <- errors.New("simulated subscription disconnect"):
						default:
						}
					}
				case <-env.ctx.Done():
					return
				}
			}
		}()

		var wg sync.WaitGroup
		wg.Add(1)
		numBlocksReceived := atomic.Uint32{}
		newBlockReceiver := make(chan *types.Block, 1000)
		const stopAfterNBlocks = 500

		go func() {
			defer wg.Done()
			for {
				select {
				case <-newBlockReceiver:
					if numBlocksReceived.Add(1) >= stopAfterNBlocks {
						return
					}
				case <-env.ctx.Done():
					return
				}
			}
		}()

		// Disable client refresh to isolate the drop logic.
		subscriberConfig := &SubscriberConfig{
			UpdateClientSetInterval: 1 * time.Hour,
			Logger:                  &NoOpLogger{},
		}
		blockSubscriber := NewBlockSubscriber(env.ctx, newBlockReceiver, env.getHealthyClientsFunc, subscriberConfig)

		wg.Wait()

		blockSubscriber.mu.RLock()
		activeSubs := len(blockSubscriber.clientSet)
		blockSubscriber.mu.RUnlock()

		assert.Less(t, activeSubs, 200, "Should have fewer active subscriptions due to failures.")

		blockSubscriber.Close()
	})

	t.Run("should self-heal by re-subscribing to failed clients", func(t *testing.T) {
		env := setupResilienceTest(t, 50)
		defer env.cancel()

		// Chaos goroutine needs its own context so we can stop it independently.
		chaosCtx, chaosCancel := context.WithCancel(env.ctx)
		defer chaosCancel()

		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			rand.Seed(time.Now().UnixNano())
			for {
				select {
				case <-ticker.C:
					clientIndex := rand.Intn(len(env.ethClients))
					clientToFail := env.ethClients[clientIndex]
					if errCh := env.subscriptionErrorChs[clientToFail]; errCh != nil {
						select {
						case errCh <- errors.New("simulated subscription disconnect"):
						default:
						}
					}
				case <-chaosCtx.Done():
					return
				}
			}
		}()

		var wg sync.WaitGroup
		wg.Add(1)
		numBlocksReceived := atomic.Uint32{}
		newBlockReceiver := make(chan *types.Block, 1000)
		const stopAfterNBlocks = 500

		go func() {
			defer wg.Done()
			for {
				select {
				case <-newBlockReceiver:
					if numBlocksReceived.Add(1) >= stopAfterNBlocks {
						return
					}
				case <-env.ctx.Done():
					return
				}
			}
		}()

		// Enable a fast client refresh to test self-healing.
		subscriberConfig := &SubscriberConfig{
			UpdateClientSetInterval: 150 * time.Millisecond,
			Logger:                  &NoOpLogger{},
		}
		blockSubscriber := NewBlockSubscriber(env.ctx, newBlockReceiver, env.getHealthyClientsFunc, subscriberConfig)

		wg.Wait()
		chaosCancel()                                            // Stop injecting failures.
		time.Sleep(subscriberConfig.UpdateClientSetInterval * 2) // Allow time for a final refresh.

		blockSubscriber.mu.RLock()
		activeSubs := len(blockSubscriber.clientSet)
		blockSubscriber.mu.RUnlock()

		assert.Equal(t, 50, activeSubs, "Subscriber should have self-healed back to full capacity.")

		blockSubscriber.Close()
	})
}
