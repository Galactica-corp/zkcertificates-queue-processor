package queueprocessor

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	merkleproto "github.com/Galactica-corp/merkle-proof-service/gen/galactica/merkle"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	eventbus "github.com/jilio/ebu"
	"github.com/galactica-corp/zkcertificates-queue-processor/zkregistry"
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
	ZkCertificateLeafHash common.Hash
	Guardian              common.Address
	Operation             uint8
	QueueIndex            *big.Int
}

type Service struct {
	name                string
	client              *ethclient.Client
	contractAddress     common.Address
	registryContract    *zkregistry.ZkCertificateRegistry
	eventBus            *eventbus.EventBus
	currentQueuePointer *big.Int
	queue               []QueuedOperation
	mu                  sync.Mutex
	wg                  sync.WaitGroup
	ctx                 context.Context
	cancel              context.CancelFunc
	isRunning           bool
	checkInterval       time.Duration
	merkleClient        merkleproto.QueryClient
	merkleServiceURL    string
	merkleServiceTLS    bool
	privateKey          *ecdsa.PrivateKey
	chainID             *big.Int
}

func NewService(name string, client *ethclient.Client, contractAddress common.Address, eventBus *eventbus.EventBus) (*Service, error) {
	registryContract, err := zkregistry.NewZkCertificateRegistry(contractAddress, client)
	if err != nil {
		return nil, err
	}

	// Get chain ID
	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	// Create merkle proof client; TODO: move to env
	merkleServiceURL := "grpc-merkle.lookhere.tech:443"
	merkleServiceTLS := true

	merkleProofClient, err := merkle.ConnectToMerkleProofService(merkleServiceURL, merkleServiceTLS)
	if err != nil {
		return nil, err
	}

	s := &Service{
		name:                name,
		client:              client,
		contractAddress:     contractAddress,
		registryContract:    registryContract,
		eventBus:            eventBus,
		currentQueuePointer: big.NewInt(0),
		queue:               make([]QueuedOperation, 0),
		checkInterval:       10 * time.Second,
		merkleClient:        merkleProofClient,
		merkleServiceURL:    merkleServiceURL,
		merkleServiceTLS:    merkleServiceTLS,
		chainID:             chainID,
	}

	return s, nil
}

func (s *Service) Start() {
	s.mu.Lock()
	if s.isRunning {
		s.mu.Unlock()
		return
	}
	s.isRunning = true
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.mu.Unlock()

	slog.Info("Starting queue processor service", "name", s.name)

	// Subscribe to OperationQueued events
	if s.eventBus != nil {
		eventbus.Subscribe(s.eventBus, func(event OperationQueuedEvent) {
			s.handleOperationQueued(event)
		})
	}

	// Start the queue monitoring loop
	s.wg.Add(1)
	go s.monitorQueue()

	slog.Info("Queue processor service started", "name", s.name)
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.isRunning {
		s.mu.Unlock()
		return nil
	}
	s.isRunning = false
	s.mu.Unlock()

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
	s.mu.Lock()
	defer s.mu.Unlock()

	slog.Info("Received OperationQueued event",
		"hash", event.ZkCertificateLeafHash.Hex(),
		"guardian", event.Guardian.Hex(),
		"operation", event.Operation,
		"queueIndex", event.QueueIndex.String())

	// Add to internal queue
	op := QueuedOperation{
		Hash:       event.ZkCertificateLeafHash,
		Guardian:   event.Guardian,
		QueueIndex: event.QueueIndex,
		State:      event.Operation,
	}

	s.queue = append(s.queue, op)
}

func (s *Service) monitorQueue() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkAndProcessQueue()
		}
	}
}

func (s *Service) checkAndProcessQueue() {
	// Get current queue pointer from contract
	currentPointer, err := s.registryContract.CurrentQueuePointer(nil)
	if err != nil {
		slog.Error("Failed to get current queue pointer", "error", err)
		return
	}

	// Get queue length
	queueLength, err := s.registryContract.GetZkCertificateQueueLength(nil)
	if err != nil {
		slog.Error("Failed to get queue length", "error", err)
		return
	}

	slog.Info("Queue status",
		"currentPointer", currentPointer.String(),
		"queueLength", queueLength.String(),
		"hasItemsToProcess", currentPointer.Cmp(queueLength) < 0)

	// Debug: Check states of next few items
	if true { // Always show debug info for now
		for i := 0; i < 3 && new(big.Int).Add(currentPointer, big.NewInt(int64(i))).Cmp(queueLength) < 0; i++ {
			idx := new(big.Int).Add(currentPointer, big.NewInt(int64(i)))
			if hash, err := s.registryContract.ZkCertificateQueue(nil, idx); err == nil {
				if data, err := s.registryContract.ZkCertificateProcessingData(nil, hash); err == nil {
					slog.Debug("Queue item preview",
						"index", idx.String(),
						"hash", common.Bytes2Hex(hash[:]),
						"state", data.State)
				}
			}
		}
	}

	// Process any items that are ready
	s.processQueueItems(currentPointer, queueLength)
}

func (s *Service) processQueueItems(currentPointer, queueLength *big.Int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if there are items to process
	if currentPointer.Cmp(queueLength) >= 0 {
		return
	}

	// Get the next item from the contract
	nextItemHash, err := s.registryContract.ZkCertificateQueue(nil, currentPointer)
	if err != nil {
		slog.Error("Failed to get queue item", "index", currentPointer.String(), "error", err)
		return
	}

	// Get processing data for the certificate
	certData, err := s.registryContract.ZkCertificateProcessingData(nil, nextItemHash)
	if err != nil {
		slog.Error("Failed to get certificate data", "hash", common.Bytes2Hex(nextItemHash[:]), "error", err)
		return
	}

	slog.Info("Found item to process",
		"index", currentPointer.String(),
		"hash", common.Bytes2Hex(nextItemHash[:]),
		"state", certData.State,
		"guardian", certData.Guardian.Hex())

	// Check if it's in turn to be processed
	isInTurn, err := s.registryContract.IsZkCertificateInTurn(nil, nextItemHash)
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
		slog.Info("Processing issuance", "hash", common.Bytes2Hex(nextItemHash[:]))
		// For issuance, we need an empty leaf proof since we're adding a new certificate
		s.processIssuance(nextItemHash, certData.QueueIndex)

	case CertificateStateRevocationQueued:
		slog.Info("Processing revocation", "hash", common.Bytes2Hex(nextItemHash[:]))
		// For revocation, we need the proof of the existing certificate
		s.processRevocation(nextItemHash, certData.QueueIndex)

	default:
		slog.Warn("Unknown certificate state", "state", certData.State)
	}
}

// SetCheckInterval allows configuring how often the queue is checked
func (s *Service) SetCheckInterval(interval time.Duration) {
	s.checkInterval = interval
}

// GetQueueLength returns the current internal queue length
func (s *Service) GetQueueLength() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
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

	s.mu.Lock()
	s.privateKey = privateKey
	s.mu.Unlock()

	// Log the address for debugging
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	slog.Info("Private key configured", "address", address.Hex())

	return nil
}

// processIssuance handles certificate issuance by getting an empty leaf proof
func (s *Service) processIssuance(zkCertHash [32]byte, queueIndex *big.Int) {
	// For issuance, we need to get an empty leaf proof
	emptyIndex, proof, err := merkle.GetEmptyLeafProof(s.ctx, s.merkleClient, s.contractAddress.Hex())
	if err != nil {
		slog.Error("Failed to get empty leaf proof",
			"hash", common.Bytes2Hex(zkCertHash[:]),
			"error", err)
		return
	}

	slog.Info("Retrieved empty leaf proof for issuance",
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

	tx, err := s.registryContract.ProcessNextOperation(auth, big.NewInt(int64(emptyIndex)), zkCertHash, merkleProof)
	if err != nil {
		slog.Error("Failed to process issuance operation", "error", err)
		return
	}

	slog.Info("Submitted processNextOperation transaction for issuance",
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"txHash", tx.Hash().Hex(),
		"leafIndex", emptyIndex)
}

// processRevocation handles certificate revocation by getting proof of existing leaf
func (s *Service) processRevocation(zkCertHash [32]byte, queueIndex *big.Int) {
	// Convert zkCertHash to string for the merkle proof service
	leafValue := new(big.Int).SetBytes(zkCertHash[:])
	leafStr := leafValue.String()

	// For revocation, we need to find where this certificate exists in the tree
	proof, err := merkle.GetProof(s.ctx, s.merkleClient, s.contractAddress.Hex(), leafStr)
	if err != nil {
		slog.Error("Failed to get merkle proof for revocation",
			"hash", common.Bytes2Hex(zkCertHash[:]),
			"leafDecimal", leafStr,
			"error", err)
		return
	}

	slog.Info("Retrieved merkle proof for revocation",
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

	tx, err := s.registryContract.ProcessNextOperation(auth, big.NewInt(int64(proof.LeafIndex)), zkCertHash, merkleProof)
	if err != nil {
		slog.Error("Failed to process revocation operation", "error", err)
		return
	}

	slog.Info("Submitted processNextOperation transaction for revocation",
		"hash", common.Bytes2Hex(zkCertHash[:]),
		"txHash", tx.Hash().Hex(),
		"leafIndex", proof.LeafIndex)
}
