package evm

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Option is a function that configures the Service
type Option func(*Service)

// WithPollingInterval sets the polling interval for event checking
func WithPollingInterval(interval time.Duration) Option {
	return func(s *Service) {
		if interval > 0 {
			s.pollingInterval = interval
		}
	}
}

// WithDefaultStartBlock sets the default starting block for new contracts
func WithDefaultStartBlock(blockNumber *big.Int) Option {
	return func(s *Service) {
		if blockNumber != nil {
			s.defaultStartBlock = new(big.Int).Set(blockNumber)
		}
	}
}

// WithInitialContracts registers contracts to monitor from the start with their starting blocks
func WithInitialContracts(contracts map[string]*big.Int) Option {
	return func(s *Service) {
		for addr, startBlock := range contracts {
			if common.IsHexAddress(addr) {
				address := common.HexToAddress(addr)
				if startBlock == nil {
					startBlock = s.defaultStartBlock
				}
				s.contracts[address] = &ContractInfo{
					Address:      address,
					StartBlock:   new(big.Int).Set(startBlock),
					CurrentBlock: new(big.Int).Set(startBlock),
				}
			}
		}
	}
}

// WithBlockConfirmations sets the number of block confirmations to wait
// before considering an event final
func WithBlockConfirmations(confirmations uint64) Option {
	return func(s *Service) {
		s.confirmations = confirmations
	}
}

// WithEventFilter allows filtering of specific event signatures
// Only events matching these signatures will be published
func WithEventFilter(signatures ...common.Hash) Option {
	return func(s *Service) {
		for _, sig := range signatures {
			s.eventFilters[sig] = true
		}
	}
}

// WithMaxBlockRange sets the maximum number of blocks to query in a single request
// This is useful for RPC providers that have limits on block range
func WithMaxBlockRange(maxBlocks *big.Int) Option {
	return func(s *Service) {
		if maxBlocks != nil && maxBlocks.Cmp(big.NewInt(1)) > 0 {
			s.maxBlockRange = new(big.Int).Set(maxBlocks)
		}
	}
}
