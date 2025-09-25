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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
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

	slog.Info("Received OperationQueued event",
		"registry", registry.Name,
		"hash", event.ZkCertificateLeafHash.Hex(),
		"guardian", event.Guardian.Hex(),
		"operation", event.Operation,
		"queueIndex", event.QueueIndex.String())

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

	slog.Info("Queue status",
		"registry", registry.Name,
		"currentPointer", currentPointer.String(),
		"queueLength", queueLength.String(),
		"hasItemsToProcess", currentPointer.Cmp(queueLength) < 0)

	// Debug: Check states of next few items
	if true { // Always show debug info for now
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

	slog.Info("Found item to process",
		"registry", registry.Name,
		"index", currentPointer.String(),
		"hash", common.Bytes2Hex(nextItemHash[:]),
		"state", certData.State,
		"guardian", certData.Guardian.Hex())

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
		slog.Info("Processing issuance", "registry", registry.Name, "hash", common.Bytes2Hex(nextItemHash[:]))
		// For issuance, we need an empty leaf proof since we're adding a new certificate
		s.processIssuance(address, registry, nextItemHash, certData.QueueIndex)

	case CertificateStateRevocationQueued:
		slog.Info("Processing revocation", "registry", registry.Name, "hash", common.Bytes2Hex(nextItemHash[:]))
		// For revocation, we need the proof of the existing certificate
		s.processRevocation(address, registry, nextItemHash, certData.QueueIndex)

	default:
		slog.Warn("Unknown certificate state", "state", certData.State)
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

// processIssuance handles certificate issuance by getting an empty leaf proof
func (s *Service) processIssuance(address common.Address, registry *Registry, zkCertHash [32]byte, queueIndex *big.Int) {
	// For issuance, we need to get an empty leaf proof
	emptyIndex, proof, err := merkle.GetEmptyLeafProof(s.ctx, s.merkleClient, address.Hex())
	if err != nil {
		slog.Error("Failed to get empty leaf proof",
			"hash", common.Bytes2Hex(zkCertHash[:]),
			"error", err)
		return
	}

	slog.Info("Retrieved empty leaf proof for issuance",
		"registry", registry.Name,
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"emptyLeafIndex", emptyIndex,
		"pathLength", len(proof.Path))

	// Convert proof paths to [][32]byte array
	merkleProof := make([][32]byte, len(proof.Path))
	for i, pathElement := range proof.Path {
		bytes := pathElement.Value.Bytes32()
		merkleProof[i] = bytes
	}

	slog.Info("Ready to call processNextOperation for issuance",
		"registry", registry.Name,
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"leafIndex", emptyIndex,
		"queueIndex", queueIndex.String(),
		"proofLength", len(merkleProof))

	// Submit transaction if private key is configured
	if s.privateKey == nil {
		slog.Warn("Skipping transaction submission - no private key configured")
		return
	}

	auth, err := bind.NewKeyedTransactorWithChainID(s.privateKey, s.chainID)
	if err != nil {
		slog.Error("Failed to create transactor", "error", err)
		return
	}

	tx, err := registry.Contract.ProcessNextOperation(auth, big.NewInt(int64(emptyIndex)), zkCertHash, merkleProof)
	if err != nil {
		slog.Error("Failed to process issuance operation", "error", err)
		return
	}

	slog.Info("Submitted processNextOperation transaction for issuance",
		"registry", registry.Name,
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"txHash", tx.Hash().Hex(),
		"leafIndex", emptyIndex)
}

// processRevocation handles certificate revocation by getting proof of existing leaf
func (s *Service) processRevocation(address common.Address, registry *Registry, zkCertHash [32]byte, queueIndex *big.Int) {
	// Convert zkCertHash to string for the merkle proof service
	leafValue := new(big.Int).SetBytes(zkCertHash[:])
	leafStr := leafValue.String()

	// For revocation, we need to find where this certificate exists in the tree
	proof, err := merkle.GetProof(s.ctx, s.merkleClient, address.Hex(), leafStr)
	if err != nil {
		slog.Error("Failed to get merkle proof for revocation",
			"hash", common.Bytes2Hex(zkCertHash[:]),
			"leafDecimal", leafStr,
			"error", err)
		return
	}

	slog.Info("Retrieved merkle proof for revocation",
		"registry", registry.Name,
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"leafIndex", proof.LeafIndex,
		"pathLength", len(proof.Path))

	// Convert proof paths to [][32]byte array
	merkleProof := make([][32]byte, len(proof.Path))
	for i, pathElement := range proof.Path {
		// The SDK returns TreeNode with uint256.Int values, convert to [32]byte
		bytes := pathElement.Value.Bytes32()
		merkleProof[i] = bytes
	}

	slog.Info("Ready to call processNextOperation for revocation",
		"registry", registry.Name,
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"leafIndex", proof.LeafIndex,
		"queueIndex", queueIndex.String(),
		"proofLength", len(merkleProof))

	// Submit transaction if private key is configured
	if s.privateKey == nil {
		slog.Warn("Skipping transaction submission - no private key configured")
		return
	}

	auth, err := bind.NewKeyedTransactorWithChainID(s.privateKey, s.chainID)
	if err != nil {
		slog.Error("Failed to create transactor", "error", err)
		return
	}

	tx, err := registry.Contract.ProcessNextOperation(auth, big.NewInt(int64(proof.LeafIndex)), zkCertHash, merkleProof)
	if err != nil {
		slog.Error("Failed to process revocation operation", "error", err)
		return
	}

	slog.Info("Submitted processNextOperation transaction for revocation",
		"registry", registry.Name,
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"txHash", tx.Hash().Hex(),
		"leafIndex", proof.LeafIndex)
}
