package logging

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	sentryutil "github.com/galactica-corp/zkcertificates-queue-processor/sentry"
)

// OperationBuilder accumulates context throughout an operation lifecycle
type OperationBuilder struct {
	event     OperationEvent
	startTime time.Time

	// Timing for sub-operations
	merkleStart time.Time
	txStart     time.Time
}

// NewOperationBuilder creates a new builder for tracking an operation
func NewOperationBuilder(operationType string) *OperationBuilder {
	return &OperationBuilder{
		event: OperationEvent{
			OperationID:   generateOperationID(),
			OperationType: operationType,
		},
		startTime: time.Now(),
	}
}

// WithRegistry adds registry context
func (b *OperationBuilder) WithRegistry(name, address string) *OperationBuilder {
	b.event.Registry = &RegistryContext{
		Name:    name,
		Address: address,
	}
	return b
}

// WithCertificate adds certificate context
func (b *OperationBuilder) WithCertificate(leafHash, guardian, queueIndex string) *OperationBuilder {
	b.event.Certificate = &CertificateContext{
		LeafHash:   leafHash,
		Guardian:   guardian,
		QueueIndex: queueIndex,
	}
	return b
}

// StartMerkleProof marks the start of merkle proof fetching
func (b *OperationBuilder) StartMerkleProof() *OperationBuilder {
	b.merkleStart = time.Now()
	return b
}

// WithMerkleProof adds merkle proof context (call after StartMerkleProof)
func (b *OperationBuilder) WithMerkleProof(leafIndex int64, proofLength int) *OperationBuilder {
	fetchMs := int64(0)
	if !b.merkleStart.IsZero() {
		fetchMs = time.Since(b.merkleStart).Milliseconds()
	}
	b.event.MerkleProof = &MerkleProofContext{
		LeafIndex:   leafIndex,
		ProofLength: proofLength,
		FetchMs:     fetchMs,
	}
	return b
}

// StartTransaction marks the start of transaction submission
func (b *OperationBuilder) StartTransaction() *OperationBuilder {
	b.txStart = time.Now()
	return b
}

// WithTransaction adds transaction context (call after StartTransaction)
func (b *OperationBuilder) WithTransaction(hash string, nonce uint64, attempts int) *OperationBuilder {
	submitMs := int64(0)
	if !b.txStart.IsZero() {
		submitMs = time.Since(b.txStart).Milliseconds()
	}
	b.event.Transaction = &TransactionContext{
		Hash:     hash,
		Nonce:    nonce,
		Attempts: attempts,
		SubmitMs: submitMs,
	}
	return b
}

// WithError adds error context
func (b *OperationBuilder) WithError(message, phase string) *OperationBuilder {
	b.event.Error = &ErrorContext{
		Message: message,
		Phase:   phase,
	}
	return b
}

// EmitSuccess logs the operation as successful
func (b *OperationBuilder) EmitSuccess() {
	b.event.Outcome = OutcomeSuccess
	b.emit()
}

// EmitFailure logs the operation as failed and sends to Sentry
func (b *OperationBuilder) EmitFailure() {
	b.event.Outcome = OutcomeFailure
	b.emit()
	b.sendToSentry()
}

// EmitSkipped logs the operation as skipped
func (b *OperationBuilder) EmitSkipped() {
	b.event.Outcome = OutcomeSkipped
	b.emit()
}

func (b *OperationBuilder) emit() {
	b.event.Timing = &TimingContext{
		TotalMs: time.Since(b.startTime).Milliseconds(),
	}

	// Build slog attributes from the event
	attrs := []any{
		"operation_id", b.event.OperationID,
		"operation_type", b.event.OperationType,
		"outcome", b.event.Outcome,
	}

	if b.event.Registry != nil {
		attrs = append(attrs,
			"registry_name", b.event.Registry.Name,
			"registry_address", b.event.Registry.Address,
		)
	}

	if b.event.Certificate != nil {
		attrs = append(attrs,
			"cert_leaf_hash", b.event.Certificate.LeafHash,
		)
		if b.event.Certificate.Guardian != "" {
			attrs = append(attrs, "cert_guardian", b.event.Certificate.Guardian)
		}
		if b.event.Certificate.QueueIndex != "" {
			attrs = append(attrs, "cert_queue_index", b.event.Certificate.QueueIndex)
		}
	}

	if b.event.MerkleProof != nil {
		attrs = append(attrs,
			"merkle_leaf_index", b.event.MerkleProof.LeafIndex,
			"merkle_proof_length", b.event.MerkleProof.ProofLength,
			"merkle_fetch_ms", b.event.MerkleProof.FetchMs,
		)
	}

	if b.event.Transaction != nil {
		attrs = append(attrs,
			"tx_nonce", b.event.Transaction.Nonce,
			"tx_attempts", b.event.Transaction.Attempts,
			"tx_submit_ms", b.event.Transaction.SubmitMs,
		)
		if b.event.Transaction.Hash != "" {
			attrs = append(attrs, "tx_hash", b.event.Transaction.Hash)
		}
	}

	if b.event.Timing != nil {
		attrs = append(attrs, "total_ms", b.event.Timing.TotalMs)
	}

	if b.event.Error != nil {
		attrs = append(attrs,
			"error_message", b.event.Error.Message,
			"error_phase", b.event.Error.Phase,
		)
	}

	slog.Info("operation_completed", attrs...)
}

// generateOperationID creates a short unique identifier
func generateOperationID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sendToSentry sends the wide event to Sentry for error tracking
func (b *OperationBuilder) sendToSentry() {
	// Build the wide event map for Sentry
	event := map[string]any{
		"operation_id":   b.event.OperationID,
		"operation_type": b.event.OperationType,
		"outcome":        b.event.Outcome,
	}

	if b.event.Registry != nil {
		event["registry"] = map[string]any{
			"name":    b.event.Registry.Name,
			"address": b.event.Registry.Address,
		}
	}

	if b.event.Certificate != nil {
		event["certificate"] = map[string]any{
			"leaf_hash":   b.event.Certificate.LeafHash,
			"guardian":    b.event.Certificate.Guardian,
			"queue_index": b.event.Certificate.QueueIndex,
		}
	}

	if b.event.MerkleProof != nil {
		event["merkle_proof"] = map[string]any{
			"leaf_index":   b.event.MerkleProof.LeafIndex,
			"proof_length": b.event.MerkleProof.ProofLength,
			"fetch_ms":     b.event.MerkleProof.FetchMs,
		}
	}

	if b.event.Transaction != nil {
		event["transaction"] = map[string]any{
			"hash":      b.event.Transaction.Hash,
			"nonce":     b.event.Transaction.Nonce,
			"attempts":  b.event.Transaction.Attempts,
			"submit_ms": b.event.Transaction.SubmitMs,
		}
	}

	if b.event.Timing != nil {
		event["timing"] = map[string]any{
			"total_ms": b.event.Timing.TotalMs,
		}
	}

	if b.event.Error != nil {
		event["error"] = map[string]any{
			"message": b.event.Error.Message,
			"phase":   b.event.Error.Phase,
		}
	}

	// Create error from the error context
	var err error
	if b.event.Error != nil {
		err = errors.New(b.event.Error.Message)
	} else {
		err = errors.New("operation failed")
	}

	sentryutil.CaptureWideEvent(err, event)
}
