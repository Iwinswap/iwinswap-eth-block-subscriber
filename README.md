# Iwinswap ETH Block Subscriber

A **resilient**, **concurrent**, multi-client Ethereum block subscriber for Go applications. This package manages subscriptions to multiple Ethereum nodes, handles failures gracefully, and provides a single, unified channel of the latest blocks.

---

## Overview

The `subscriber` package ensures reliable access to the Ethereum block stream, even when connected to multiple unreliable RPC nodes. It abstracts away the challenges of:

* Subscription management
* Handling disconnects or RPC failures
* Synchronizing across clients

---

## Features

* **Multi-Client Management**: Connects to and manages multiple Ethereum clients.
* **Unified Block Stream**: Publishes the latest unique block to a single channel: `(<-chan *types.Block)`.
* **Self-Healing**: Automatically reconnects dropped clients during the next refresh.
* **Resilience**: Gracefully recovers from RPC or subscription errors.
* **Configurable**: Tune intervals, buffer sizes, and timeouts.
* **Pluggable Logger**: Use your preferred logging framework via a minimal `Logger` interface.
* **Graceful Shutdown**: Ensures safe termination of background processes.

---

## Installation

```bash
go get github.com/Iwinswap/iwinswap-eth-block-subscriber/subscriber
```

---

## Example Usage

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Iwinswap/iwinswap-eth-block-subscriber/subscriber"
	ethclients "github.com/Iwinswap/iwinswap-ethclients"
	"github.com/ethereum/go-ethereum/core/types"
)

type AppLogger struct{}

func (l *AppLogger) Debug(msg string, args ...any) { log.Printf("[DEBUG] "+msg, args...) }
func (l *AppLogger) Info(msg string, args ...any)  { log.Printf("[INFO] "+msg, args...) }
func (l *AppLogger) Warn(msg string, args ...any)  { log.Printf("[WARN] "+msg, args...) }
func (l *AppLogger) Error(msg string, args ...any) { log.Printf("[ERROR] "+msg, args...) }

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	getHealthyClients := func() []ethclients.ETHClient {
		// Return a list of clients that satisfy the ethclients.ETHClient interface.
		// In a real application, this might involve health checks or service discovery.
		return []ethclients.ETHClient{} // Replace with actual client instances
	}

	config := &subscriber.SubscriberConfig{
		UpdateClientSetInterval: 1 * time.Minute,
		BlockByNumberTimeout:    15 * time.Second,
		NewBlockBuffer:          100,
		Logger:                  &AppLogger{},
	}

    blockC := make(chan *types.Block, 100)
	s := subscriber.NewBlockSubscriber(ctx, blockC, getHealthyClients, config)
	log.Println("Block subscriber started.")

	for {
		select {
		case block := <-blockC:
			fmt.Printf("New block: #%d [%s]\n", block.NumberU64(), block.Hash().Hex())
		case <-shutdown:
			log.Println("Shutting down...")
			s.Close()
			return
		case <-ctx.Done():
			log.Println("Context canceled. Exiting...")
			s.Close()
			return
		}
	}
}
```

---

## Configuration

| Field                     | Description                                      | Default       |
| ------------------------- | ------------------------------------------------ | ------------- |
| `UpdateClientSetInterval` | How often to refresh the list of healthy clients | `1m`          |
| `BlockByNumberTimeout`    | Timeout for fetching the full block via RPC      | `10s`         |
| `NewBlockBuffer`          | Channel buffer size for incoming blocks          | `100`         |
| `Logger`                  | Custom logger implementing `Logger` interface    | Stdout logger |

### Logger Interface

```go
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}
```

To use a structured logger like Zap or Logrus, simply wrap it to satisfy this interface.

---

## How It Works

### Block Processing

* Subscribes to new block headers across all clients.
* Fetches the full block from the client that sent the header.
* Delivers blocks with increasing numbers (i.e., highest number wins).
* Does not handle reorgs — this is delegated to the consumer.

### Client Management

* Periodically invokes `getHealthyClients`.
* Detects new clients and adds them to the subscription pool.
* Detects failed subscriptions and drops the client.
* Re-attempts failed clients in future refresh cycles if still considered healthy.

---

## Contributing

Bug reports and pull requests are welcome at
[https://github.com/Iwinswap/iwinswap-eth-block-subscriber](https://github.com/Iwinswap/iwinswap-eth-block-subscriber)

---

## License

MIT License
