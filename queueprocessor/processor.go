package queueprocessor

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	merkleproto "github.com/Galactica-corp/merkle-proof-service/gen/galactica/merkle"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/galactica-corp/zkcertificates-queue-processor/logging"
	"github.com/galactica-corp/zkcertificates-queue-processor/zkregistry"
	eventbus "github.com/jilio/ebu"
	"github.com/jilio/guardians-sdk/v3/pkg/merkle"
)

const (
	CertificateStateNone uint8 = iota
	CertificateStateIssuanceQueued
	CertificateStateIssuanceMerkleTreeAdded
	CertificateStateRevocationQueued
	CertificateStateRevocationMerkleTreeAdded
)

type QueuedOperation struct {
	Hash         common.Hash
	Guardian     common.Address
	QueueIndex   *big.Int
	State        uint8
	IsProcessing bool
}

type OperationQueuedEvent struct {
	RegistryAddress       common.Address
	RegistryName          string
	ZkCertificateLeafHash common.Hash
	Guardian              common.Address
	Operation             uint8
	QueueIndex            *big.Int
}

type RegistryConfig struct {
	Name    string
	Address common.Address
}

type Registry struct {
	Name                string
	Address             common.Address
	Contract            *zkregistry.ZkCertificateRegistry
	CurrentQueuePointer *big.Int
	Queue               []QueuedOperation
	Mu                  sync.Mutex
}

type Service struct {
	name             string
	client           *ethclient.Client
	registries       map[common.Address]*Registry
	eventBus         *eventbus.EventBus
	wg               sync.WaitGroup
	ctx              context.Context
	cancel           context.CancelFunc
	isRunning        bool
	checkInterval    time.Duration
	merkleClient     merkleproto.QueryClient
	merkleServiceURL string
	merkleServiceTLS bool
	privateKey       *ecdsa.PrivateKey
	chainID          *big.Int
	nonceMu          sync.Mutex
	currentNonce     *uint64
}

func NewServiceWithMultipleRegistries(name string, client *ethclient.Client, registryConfigs []RegistryConfig, eventBus *eventbus.EventBus) (*Service, error) {
	// Get chain ID
	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	// Get merkle service configuration from environment
	merkleServiceURL := os.Getenv("MERKLE_SERVICE_URL")
	if merkleServiceURL == "" {
		merkleServiceURL = "grpc-merkle-843843.galactica.com:443"
		slog.Warn("MERKLE_SERVICE_URL not set, using default", "url", merkleServiceURL)
	}

	merkleServiceTLS := true
	if tlsStr := os.Getenv("MERKLE_SERVICE_TLS"); tlsStr != "" {
		var err error
		merkleServiceTLS, err = strconv.ParseBool(tlsStr)
		if err != nil {
			slog.Warn("Invalid MERKLE_SERVICE_TLS value, using default true", "value", tlsStr)
			merkleServiceTLS = true
		}
	}

	merkleProofClient, err := merkle.ConnectToMerkleProofService(merkleServiceURL, merkleServiceTLS)
	if err != nil {
		return nil, err
	}

	// Initialize registries map
	registries := make(map[common.Address]*Registry)

	for _, config := range registryConfigs {
		registryContract, err := zkregistry.NewZkCertificateRegistry(config.Address, client)
		if err != nil {
			return nil, fmt.Errorf("failed to create registry contract for %s: %w", config.Name, err)
		}

		registries[config.Address] = &Registry{
			Name:                config.Name,
			Address:             config.Address,
			Contract:            registryContract,
			CurrentQueuePointer: big.NewInt(0),
			Queue:               make([]QueuedOperation, 0),
		}
	}

	s := &Service{
		name:             name,
		client:           client,
		registries:       registries,
		eventBus:         eventBus,
		checkInterval:    10 * time.Second,
		merkleClient:     merkleProofClient,
		merkleServiceURL: merkleServiceURL,
		merkleServiceTLS: merkleServiceTLS,
		chainID:          chainID,
	}

	return s, nil
}

// Keep the original NewService for backward compatibility
func NewService(name string, client *ethclient.Client, contractAddress common.Address, eventBus *eventbus.EventBus) (*Service, error) {
	return NewServiceWithMultipleRegistries(name, client, []RegistryConfig{
		{Name: "Default", Address: contractAddress},
	}, eventBus)
}

func (s *Service) Start() {
	if s.isRunning {
		return
	}
	s.isRunning = true
	s.ctx, s.cancel = context.WithCancel(context.Background())

	slog.Info("Starting queue processor service", "name", s.name)

	// Subscribe to OperationQueued events
	if s.eventBus != nil {
		eventbus.Subscribe(s.eventBus, func(event OperationQueuedEvent) {
			s.handleOperationQueued(event)
		})
	}

	// Start a queue monitoring loop for each registry
	for address, registry := range s.registries {
		s.wg.Add(1)
		go s.monitorRegistryQueue(address, registry)
	}

	slog.Info("Queue processor service started", "name", s.name, "registries", len(s.registries))
}

func (s *Service) Shutdown(ctx context.Context) error {
	if !s.isRunning {
		return nil
	}
	s.isRunning = false

	slog.Info("Shutting down queue processor service", "name", s.name)

	if s.cancel != nil {
		s.cancel()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	var shutdownErr error
	select {
	case <-done:
		slog.Info("Queue processor service shutdown complete", "name", s.name)
	case <-ctx.Done():
		slog.Warn("Queue processor service shutdown timeout", "name", s.name)
		shutdownErr = ctx.Err()
	}

	// The merkle client connection is managed by the SDK
	// No need to explicitly close it here

	return shutdownErr
}

func (s *Service) handleOperationQueued(event OperationQueuedEvent) {
	// Find the registry for this event
	registry, ok := s.registries[event.RegistryAddress]
	if !ok {
		slog.Error("Received event for unknown registry",
			"address", event.RegistryAddress.Hex())
		return
	}

	registry.Mu.Lock()
	defer registry.Mu.Unlock()

	// Add to registry's queue
	op := QueuedOperation{
		Hash:       event.ZkCertificateLeafHash,
		Guardian:   event.Guardian,
		QueueIndex: event.QueueIndex,
		State:      event.Operation,
	}

	registry.Queue = append(registry.Queue, op)
}

func (s *Service) monitorRegistryQueue(address common.Address, registry *Registry) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkAndProcessRegistryQueue(address, registry)
		}
	}
}

func (s *Service) checkAndProcessRegistryQueue(address common.Address, registry *Registry) {
	// Get current queue pointer from contract
	currentPointer, err := registry.Contract.CurrentQueuePointer(nil)
	if err != nil {
		slog.Error("Failed to get current queue pointer", "error", err)
		return
	}

	// Get queue length
	queueLength, err := registry.Contract.GetZkCertificateQueueLength(nil)
	if err != nil {
		slog.Error("Failed to get queue length", "error", err)
		return
	}

	slog.Debug("Queue status",
		"registry", registry.Name,
		"currentPointer", currentPointer.String(),
		"queueLength", queueLength.String(),
		"hasItemsToProcess", currentPointer.Cmp(queueLength) < 0)

	// Preview next few items in queue
	for i := 0; i < 3 && new(big.Int).Add(currentPointer, big.NewInt(int64(i))).Cmp(queueLength) < 0; i++ {
		idx := new(big.Int).Add(currentPointer, big.NewInt(int64(i)))
		if hash, err := registry.Contract.ZkCertificateQueue(nil, idx); err == nil {
			if data, err := registry.Contract.ZkCertificateProcessingData(nil, hash); err == nil {
				slog.Debug("Queue item preview",
					"registry", registry.Name,
					"index", idx.String(),
					"hash", common.Bytes2Hex(hash[:]),
					"state", data.State)
			}
		}
	}

	// Process any items that are ready
	s.processQueueItems(address, registry, currentPointer, queueLength)
}

func (s *Service) processQueueItems(address common.Address, registry *Registry, currentPointer, queueLength *big.Int) {
	registry.Mu.Lock()
	defer registry.Mu.Unlock()

	// Check if there are items to process
	if currentPointer.Cmp(queueLength) >= 0 {
		return
	}

	// Get the next item from the contract
	nextItemHash, err := registry.Contract.ZkCertificateQueue(nil, currentPointer)
	if err != nil {
		slog.Error("Failed to get queue item", "index", currentPointer.String(), "error", err)
		return
	}

	// Get processing data for the certificate
	certData, err := registry.Contract.ZkCertificateProcessingData(nil, nextItemHash)
	if err != nil {
		slog.Error("Failed to get certificate data", "hash", common.Bytes2Hex(nextItemHash[:]), "error", err)
		return
	}

	// Check if it's in turn to be processed
	isInTurn, err := registry.Contract.IsZkCertificateInTurn(nil, nextItemHash)
	if err != nil {
		slog.Error("Failed to check if certificate is in turn", "error", err)
		return
	}

	if !isInTurn {
		slog.Debug("Certificate not yet in turn", "hash", common.Bytes2Hex(nextItemHash[:]))
		return
	}

	// Process based on state
	switch certData.State {
	case CertificateStateIssuanceQueued:
		s.processIssuance(address, registry, nextItemHash, certData.QueueIndex)
	case CertificateStateRevocationQueued:
		s.processRevocation(address, registry, nextItemHash, certData.QueueIndex)
	default:
		slog.Warn("Unknown certificate state",
			"registry", registry.Name,
			"registryAddress", address.Hex(),
			"state", certData.State)
	}
}

// SetCheckInterval allows configuring how often the queue is checked
func (s *Service) SetCheckInterval(interval time.Duration) {
	s.checkInterval = interval
}

// GetQueueLength returns the total queue length across all registries
func (s *Service) GetQueueLength() int {
	total := 0
	for _, registry := range s.registries {
		registry.Mu.Lock()
		total += len(registry.Queue)
		registry.Mu.Unlock()
	}
	return total
}

// SetPrivateKey sets the private key for transaction signing
func (s *Service) SetPrivateKey(privateKeyHex string) error {
	// Remove 0x prefix if present
	if len(privateKeyHex) >= 2 && privateKeyHex[:2] == "0x" {
		privateKeyHex = privateKeyHex[2:]
	}

	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return fmt.Errorf("invalid private key: %w", err)
	}

	s.privateKey = privateKey

	// Log the address for debugging
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	slog.Info("Private key set", "address", address.Hex())

	return nil
}

// prepareTransactor creates a transactor with the next nonce
func (s *Service) prepareTransactor() (*bind.TransactOpts, uint64, error) {
	auth, err := bind.NewKeyedTransactorWithChainID(s.privateKey, s.chainID)
	if err != nil {
		return nil, 0, err
	}

	senderAddress := crypto.PubkeyToAddress(s.privateKey.PublicKey)

	s.nonceMu.Lock()
	if s.currentNonce == nil {
		nonce, err := s.client.PendingNonceAt(s.ctx, senderAddress)
		if err != nil {
			s.nonceMu.Unlock()
			return nil, 0, err
		}
		s.currentNonce = &nonce
	}
	thisNonce := *s.currentNonce
	*s.currentNonce++
	s.nonceMu.Unlock()

	auth.Nonce = big.NewInt(int64(thisNonce))
	auth.GasLimit = 10000000

	return auth, thisNonce, nil
}

// submitWithRetry wraps transaction submission with retry logic and nonce reset.
// Returns the result, final nonce used, number of attempts, and any error.
func (s *Service) submitWithRetry(
	registry *Registry,
	submitFn func(auth *bind.TransactOpts) (interface{}, error),
	maxRetries int,
) (result interface{}, finalNonce uint64, attempts int, err error) {
	var lastErr error
	var lastNonce uint64
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Reset nonce before retry
			s.nonceMu.Lock()
			s.currentNonce = nil
			s.nonceMu.Unlock()

			backoff := time.Duration(1<<attempt) * time.Second
			time.Sleep(backoff)
		}

		auth, nonce, prepErr := s.prepareTransactor()
		if prepErr != nil {
			lastErr = prepErr
			continue
		}
		lastNonce = nonce

		result, submitErr := submitFn(auth)
		if submitErr == nil {
			return result, nonce, attempt + 1, nil
		}

		lastErr = submitErr
	}

	// All retries failed, reset nonce for next call
	s.nonceMu.Lock()
	s.currentNonce = nil
	s.nonceMu.Unlock()

	return nil, lastNonce, maxRetries, lastErr
}

// processIssuance handles certificate issuance by getting an empty leaf proof
func (s *Service) processIssuance(address common.Address, registry *Registry, zkCertHash [32]byte, queueIndex *big.Int) {
	op := logging.NewOperationBuilder(logging.OperationIssuance).
		WithRegistry(registry.Name, address.Hex()).
		WithCertificate(common.Bytes2Hex(zkCertHash[:]), "", queueIndex.String())

	// For issuance, we need to get an empty leaf proof
	op.StartMerkleProof()
	emptyIndex, proof, err := merkle.GetEmptyLeafProof(s.ctx, s.merkleClient, address.Hex())
	if err != nil {
		op.WithError(err.Error(), "merkle_proof").EmitFailure()
		return
	}
	op.WithMerkleProof(int64(emptyIndex), len(proof.Path))

	// Convert proof paths to [][32]byte array
	merkleProof := make([][32]byte, len(proof.Path))
	for i, pathElement := range proof.Path {
		bytes := pathElement.Value.Bytes32()
		merkleProof[i] = bytes
	}

	// Submit transaction if private key is configured
	if s.privateKey == nil {
		op.WithError("no private key configured", "config").EmitSkipped()
		return
	}

	// Submit with retry logic
	op.StartTransaction()
	result, nonce, attempts, err := s.submitWithRetry(registry, func(auth *bind.TransactOpts) (interface{}, error) {
		return registry.Contract.ProcessNextOperation(auth, big.NewInt(int64(emptyIndex)), zkCertHash, merkleProof)
	}, 3)

	if err != nil {
		op.WithTransaction("", nonce, attempts).
			WithError(err.Error(), "transaction").
			EmitFailure()
		return
	}

	tx := result.(*types.Transaction)
	op.WithTransaction(tx.Hash().Hex(), nonce, attempts).EmitSuccess()
}

// processRevocation handles certificate revocation by getting proof of existing leaf
func (s *Service) processRevocation(address common.Address, registry *Registry, zkCertHash [32]byte, queueIndex *big.Int) {
	op := logging.NewOperationBuilder(logging.OperationRevocation).
		WithRegistry(registry.Name, address.Hex()).
		WithCertificate(common.Bytes2Hex(zkCertHash[:]), "", queueIndex.String())

	// Convert zkCertHash to string for the merkle proof service
	leafValue := new(big.Int).SetBytes(zkCertHash[:])
	leafStr := leafValue.String()

	// For revocation, we need to find where this certificate exists in the tree
	op.StartMerkleProof()
	proof, err := merkle.GetProof(s.ctx, s.merkleClient, address.Hex(), leafStr)
	if err != nil {
		op.WithError(err.Error(), "merkle_proof").EmitFailure()
		return
	}
	op.WithMerkleProof(int64(proof.LeafIndex), len(proof.Path))

	// Convert proof paths to [][32]byte array
	merkleProof := make([][32]byte, len(proof.Path))
	for i, pathElement := range proof.Path {
		bytes := pathElement.Value.Bytes32()
		merkleProof[i] = bytes
	}

	// Submit transaction if private key is configured
	if s.privateKey == nil {
		op.WithError("no private key configured", "config").EmitSkipped()
		return
	}

	// Submit with retry logic
	op.StartTransaction()
	result, nonce, attempts, err := s.submitWithRetry(registry, func(auth *bind.TransactOpts) (interface{}, error) {
		return registry.Contract.ProcessNextOperation(auth, big.NewInt(int64(proof.LeafIndex)), zkCertHash, merkleProof)
	}, 3)

	if err != nil {
		op.WithTransaction("", nonce, attempts).
			WithError(err.Error(), "transaction").
			EmitFailure()
		return
	}

	tx := result.(*types.Transaction)
	op.WithTransaction(tx.Hash().Hex(), nonce, attempts).EmitSuccess()
}
