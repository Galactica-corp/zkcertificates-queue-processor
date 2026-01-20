package logging

// Operation types
const (
	OperationIssuance   = "issuance"
	OperationRevocation = "revocation"
)

// Outcome types
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeSkipped = "skipped"
)

// RegistryContext contains registry information
type RegistryContext struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// CertificateContext contains certificate information
type CertificateContext struct {
	LeafHash   string `json:"leaf_hash"`
	Guardian   string `json:"guardian,omitempty"`
	QueueIndex string `json:"queue_index,omitempty"`
}

// MerkleProofContext contains merkle proof fetch information
type MerkleProofContext struct {
	LeafIndex   int64 `json:"leaf_index"`
	ProofLength int   `json:"proof_length"`
	FetchMs     int64 `json:"fetch_ms"`
}

// TransactionContext contains transaction submission information
type TransactionContext struct {
	Hash     string `json:"hash,omitempty"`
	Nonce    uint64 `json:"nonce"`
	Attempts int    `json:"attempts"`
	SubmitMs int64  `json:"submit_ms"`
}

// TimingContext contains overall timing information
type TimingContext struct {
	TotalMs int64 `json:"total_ms"`
}

// ErrorContext contains error information
type ErrorContext struct {
	Message string `json:"message"`
	Phase   string `json:"phase"` // "merkle_proof", "transaction", etc.
}

// OperationEvent is the complete wide event structure
type OperationEvent struct {
	OperationID   string              `json:"operation_id"`
	OperationType string              `json:"operation_type"`
	Outcome       string              `json:"outcome"`
	Registry      *RegistryContext    `json:"registry,omitempty"`
	Certificate   *CertificateContext `json:"certificate,omitempty"`
	MerkleProof   *MerkleProofContext `json:"merkle_proof,omitempty"`
	Transaction   *TransactionContext `json:"transaction,omitempty"`
	Timing        *TimingContext      `json:"timing,omitempty"`
	Error         *ErrorContext       `json:"error,omitempty"`
}
