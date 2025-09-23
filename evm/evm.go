package evm

import (
	"context"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	eventbus "github.com/jilio/ebu"
)

// ContractEvent represents a blockchain event to be published to the event bus
type ContractEvent struct {
	Contract common.Address
	Event    types.Log
}

// ContractInfo stores information about a monitored contract
type ContractInfo struct {
	Address      common.Address
	StartBlock   *big.Int
	CurrentBlock *big.Int // Track last processed block
}

// Service handles EVM blockchain interactions
type Service struct {
	client            *ethclient.Client
	rpcURL            string
	name              string
	eventBus          *eventbus.EventBus
	contracts         map[common.Address]*ContractInfo
	defaultStartBlock *big.Int
	pollingInterval   time.Duration
	confirmations     uint64
	eventFilters      map[common.Hash]bool
	maxBlockRange     *big.Int // Maximum blocks per query (RPC limit)
	mu                sync.RWMutex
	cancel            context.CancelFunc
	wg                sync.WaitGroup
}

// NewService creates a new EVM service
func NewService(name, rpcURL string, eventBus *eventbus.EventBus, opts ...Option) (*Service, error) {
	s := &Service{
		name:              name,
		rpcURL:            rpcURL,
		eventBus:          eventBus,
		contracts:         make(map[common.Address]*ContractInfo),
		defaultStartBlock: big.NewInt(0),   // Default to genesis block
		pollingInterval:   5 * time.Second, // Default block time
		confirmations:     0,               // Default to no confirmations
		eventFilters:      make(map[common.Hash]bool),
		maxBlockRange:     big.NewInt(10000), // Default RPC limit
	}

	// Apply options
	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

// RegisterContract adds a contract address to monitor for events starting from a specific block
func (s *Service) RegisterContract(address common.Address, startBlock *big.Int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if startBlock == nil {
		startBlock = new(big.Int).Set(s.defaultStartBlock)
	}

	s.contracts[address] = &ContractInfo{
		Address:      address,
		StartBlock:   new(big.Int).Set(startBlock),
		CurrentBlock: new(big.Int).Set(startBlock),
	}
	slog.Info("registered contract", "service", s.name, "address", address.Hex(), "startBlock", startBlock)
}

// RegisterContractFromCurrent adds a contract address to monitor starting from the current block
func (s *Service) RegisterContractFromCurrent(address common.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Get current block number
	var currentBlock *big.Int
	if s.client != nil {
		header, err := s.client.HeaderByNumber(context.Background(), nil)
		if err == nil {
			currentBlock = header.Number
		} else {
			currentBlock = big.NewInt(0)
		}
	} else {
		currentBlock = big.NewInt(0)
	}

	s.contracts[address] = &ContractInfo{
		Address:      address,
		StartBlock:   new(big.Int).Set(currentBlock),
		CurrentBlock: new(big.Int).Set(currentBlock),
	}
	slog.Info("registered contract from current", "service", s.name, "address", address.Hex(), "startBlock", currentBlock)
}

// UnregisterContract removes a contract address from monitoring
func (s *Service) UnregisterContract(address common.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contracts, address)
	slog.Info("unregistered contract", "service", s.name, "address", address.Hex())
}

// Start begins the EVM service
func (s *Service) Start() {
	slog.Info(s.name+" service starting", "rpc", s.rpcURL)

	// Connect to EVM node
	client, err := ethclient.Dial(s.rpcURL)
	if err != nil {
		slog.Error("failed to connect to EVM node", "error", err)
		return
	}
	s.client = client

	// Get chain ID to verify connection
	chainID, err := client.ChainID(context.Background())
	if err != nil {
		slog.Error("failed to get chain ID", "error", err)
		return
	}
	slog.Info("connected to EVM", "service", s.name, "chainID", chainID)

	// Start event listener
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.listenForEvents(ctx)
}

// listenForEvents monitors registered contracts for events
func (s *Service) listenForEvents(ctx context.Context) {
	defer s.wg.Done()

	// Process each contract independently
	for {
		select {
		case <-ctx.Done():
			return
		default:
			s.mu.RLock()
			contracts := make(map[common.Address]*ContractInfo)
			for addr, info := range s.contracts {
				// Create a copy to avoid race conditions
				contracts[addr] = &ContractInfo{
					Address:      info.Address,
					StartBlock:   new(big.Int).Set(info.StartBlock),
					CurrentBlock: new(big.Int).Set(info.CurrentBlock),
				}
			}
			s.mu.RUnlock()

			if len(contracts) == 0 {
				// No contracts to monitor, wait a bit
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}

			// Process each contract independently
			for _, contractInfo := range contracts {
				select {
				case <-ctx.Done():
					return
				default:
					s.processContractEvents(ctx, contractInfo)
				}
			}

			// Wait before next scan cycle
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.pollingInterval):
			}
		}
	}
}

// processContractEvents processes events for a single contract
func (s *Service) processContractEvents(ctx context.Context, contractInfo *ContractInfo) {
	// Get latest block
	header, err := s.client.HeaderByNumber(ctx, nil)
	if err != nil {
		slog.Error("failed to get latest block", "error", err, "contract", contractInfo.Address.Hex())
		return
	}

	// Don't process if we're already at the latest block
	if contractInfo.CurrentBlock.Cmp(header.Number) >= 0 {
		return
	}

	// Process in chunks to respect RPC limits
	fromBlock := new(big.Int).Add(contractInfo.CurrentBlock, big.NewInt(1))
	toBlock := header.Number
	totalProcessedLogs := 0
	lastProcessedBlock := contractInfo.CurrentBlock

	for fromBlock.Cmp(toBlock) <= 0 {
		// Calculate chunk end block
		chunkEndBlock := new(big.Int).Add(fromBlock, new(big.Int).Sub(s.maxBlockRange, big.NewInt(1)))
		if chunkEndBlock.Cmp(toBlock) > 0 {
			chunkEndBlock = toBlock
		}

		// Create filter query for this chunk
		query := ethereum.FilterQuery{
			Addresses: []common.Address{contractInfo.Address},
			FromBlock: fromBlock,
			ToBlock:   chunkEndBlock,
		}

		// Get logs for this chunk
		slog.Debug("querying chunk",
			"contract", contractInfo.Address.Hex(),
			"fromBlock", fromBlock,
			"toBlock", chunkEndBlock,
			"chunkSize", new(big.Int).Sub(chunkEndBlock, fromBlock).Int64()+1)

		logs, err := s.client.FilterLogs(ctx, query)
		if err != nil {
			slog.Error("failed to filter logs",
				"error", err,
				"contract", contractInfo.Address.Hex(),
				"fromBlock", fromBlock,
				"toBlock", chunkEndBlock)
			// Update to last successfully processed block
			s.updateContractBlock(contractInfo.Address, lastProcessedBlock)
			return
		}

		// Process and publish each log
		for _, log := range logs {
			// Check if we should filter this event
			if len(s.eventFilters) > 0 {
				// If filters are set, check if this event matches any filter
				matchesFilter := false
				for _, topic := range log.Topics {
					if s.eventFilters[topic] {
						matchesFilter = true
						break
					}
				}
				if !matchesFilter {
					continue // Skip this event
				}
			}

			// Check confirmations requirement
			if s.confirmations > 0 {
				// Calculate confirmations
				confirmations := new(big.Int).Sub(header.Number, big.NewInt(int64(log.BlockNumber)))
				if confirmations.Cmp(big.NewInt(int64(s.confirmations))) < 0 {
					// Not enough confirmations yet, skip this event for now
					continue
				}
			}

			event := ContractEvent{
				Contract: log.Address,
				Event:    log,
			}
			eventbus.Publish(s.eventBus, event)
			slog.Debug("published contract event",
				"contract", log.Address.Hex(),
				"block", log.BlockNumber,
				"tx", log.TxHash.Hex())
		}

		totalProcessedLogs += len(logs)
		lastProcessedBlock = chunkEndBlock

		// Update progress after each chunk
		s.updateContractBlock(contractInfo.Address, chunkEndBlock)

		// Move to next chunk
		fromBlock = new(big.Int).Add(chunkEndBlock, big.NewInt(1))
	}

	if totalProcessedLogs > 0 {
		slog.Info("processed events",
			"contract", contractInfo.Address.Hex(),
			"fromBlock", contractInfo.CurrentBlock,
			"toBlock", header.Number,
			"eventCount", totalProcessedLogs)
	}
}

// updateContractBlock updates the current block for a contract
func (s *Service) updateContractBlock(address common.Address, blockNumber *big.Int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, exists := s.contracts[address]; exists {
		info.CurrentBlock = new(big.Int).Set(blockNumber)
	}
}

// Shutdown gracefully stops the EVM service
func (s *Service) Shutdown(ctx context.Context) error {
	slog.Info(s.name + " service shutting down")

	if s.cancel != nil {
		s.cancel()
	}

	// Wait for event listener to stop
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Clean shutdown
	case <-ctx.Done():
		// Timeout
		slog.Warn("timeout waiting for event listener to stop")
	}

	if s.client != nil {
		s.client.Close()
	}

	return nil
}

// GetChainID returns the chain ID of the connected network
func (s *Service) GetChainID() (*big.Int, error) {
	if s.client == nil {
		return nil, ethereum.NotFound
	}
	return s.client.ChainID(context.Background())
}
