# ZK Certificates Queue Processor - Queue Processor Service

A Go-based service that monitors and processes zkCertificate operations from the Galactica Network smart contracts.

## Overview

ZK Certificates Queue Processor implements the Queue Processor specification for the Galactica Network. It:

- Monitors the certificate queue for pending operations
- Retrieves merkle proofs from the merkle proof service
- Processes certificate issuance and revocation operations
- Submits transactions to update the on-chain merkle tree

## Architecture

The service consists of several components:

1. **EVM Service**: Monitors blockchain events from smart contracts
2. **Queue Processor**: Processes queued certificate operations
3. **Event Bus**: Internal communication between services
4. **HTTP Server**: Health check and monitoring endpoints

## Prerequisites

- Go 1.21 or higher
- Access to a Galactica Network RPC endpoint
- Access to a Merkle Proof Service instance
- A funded wallet for submitting transactions (if running as processor)

## Configuration

Copy `.env.example` to `.env` and configure:

```bash
cp .env.example .env
```

### Required Environment Variables

- `CONTRACT_ADDRESS`: The zkCertificateRegistry contract address to monitor
- `EVM_RPC_URL`: Ethereum RPC endpoint (defaults to Galactica Cassiopeia testnet)

### Optional Environment Variables

- `PRIVATE_KEY`: Private key for submitting transactions (required for processing operations)
- `PORT`: HTTP server port (defaults to 8080)

Note: The service automatically reads the contract's initialization block from the chain and starts scanning from there.

## Installation

```bash
# Clone the repository
git clone https://github.com/galactica-corp/zkcertificates-queue-processor.git
cd ZK Certificates Queue Processor

# Install dependencies
go mod download

# Build the binary
go build -o ZK Certificates Queue Processor .
```

## Running

### Development Mode (Read-Only)

To run without processing transactions:

```bash
go run main.go
```

### Production Mode (With Transaction Processing)

To run with transaction processing enabled:

```bash
export PRIVATE_KEY=0xYOUR_PRIVATE_KEY_HERE
go run main.go
```

### Using the Binary

```bash
./ZK Certificates Queue Processor
```

## Queue Processing Flow

1. **Guardians** add certificates to the queue using `addOperationToQueue()`
2. **Queue Processor** monitors the queue and for each pending operation:
   - For issuance: Gets an empty leaf proof from the merkle service
   - For revocation: Gets the existing leaf proof from the merkle service
   - Calls `processNextOperation()` with the merkle proof
   - The contract updates the merkle tree and advances the queue pointer

## Development

### Project Structure

```
ZK Certificates Queue Processor/
├── main.go              # Application entry point
├── service/             # Service management framework
├── evm/                 # Ethereum event monitoring
├── queueprocessor/      # Queue processing logic
├── server/              # HTTP server
└── zkregistry/          # Smart contract bindings
```

### Adding Contract Bindings

To regenerate contract bindings:

```bash
abigen --abi zkregistry/abi.json --pkg zkregistry --type ZkCertificateRegistry --out zkregistry/zkcertificate_registry.go
```

## Monitoring

The service provides health check endpoints:

- `GET /health` - Basic health check
- `GET /metrics` - Service metrics (if enabled)

Logs are output in structured JSON format for easy parsing and monitoring.

## Testing

Run tests with:

```bash
go test ./...
```

## References

- [Queue Processor Specification](Queue%20Processor%20Specification.md)
- [Galactica Documentation](https://docs.galactica.com)
- [Contract Explorer](https://galactica-cassiopeia.explorer.alchemy.com)

## License

[License information here]
