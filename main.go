package main

import (
	"log/slog"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/galactica-corp/zkcertificates-queue-processor/evm"
	"github.com/galactica-corp/zkcertificates-queue-processor/queueprocessor"
	"github.com/galactica-corp/zkcertificates-queue-processor/server"
	"github.com/galactica-corp/zkcertificates-queue-processor/service"
	"github.com/galactica-corp/zkcertificates-queue-processor/zkregistry"
	eventbus "github.com/jilio/ebu"
)

type AppStartEvent struct{}

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	evmRPCURL := os.Getenv("EVM_RPC_URL")
	if evmRPCURL == "" {
		evmRPCURL = "https://galactica-cassiopeia.g.alchemy.com/public"
		slog.Warn("EVM_RPC_URL not set, using default", "url", evmRPCURL)
	}

	certificateRegistryAddress := os.Getenv("CONTRACT_ADDRESS")
	if certificateRegistryAddress == "" {
		slog.Error("CONTRACT_ADDRESS environment variable is required")
		os.Exit(1)
	}

	privateKey := os.Getenv("PRIVATE_KEY")

	// Connect to Ethereum client
	client, err := ethclient.Dial(evmRPCURL)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	certificateRegistry, err := zkregistry.NewZkCertificateRegistry(common.HexToAddress(certificateRegistryAddress), client)
	if err != nil {
		panic(err)
	}

	bus := eventbus.New()

	eventbus.Subscribe(bus, func(event evm.ContractEvent) {
		if event.Contract == common.HexToAddress(certificateRegistryAddress) {
			if operationQueued, err := certificateRegistry.ParseOperationQueued(event.Event); err == nil {
				slog.Info("OperationQueued event",
					"hash", common.Bytes2Hex(operationQueued.ZkCertificateLeafHash[:]),
					"guardian", operationQueued.Guardian.Hex(),
					"operation", operationQueued.Operation,
					"queueIndex", operationQueued.QueueIndex.String())

				eventbus.Publish(bus, queueprocessor.OperationQueuedEvent{
					ZkCertificateLeafHash: operationQueued.ZkCertificateLeafHash,
					Guardian:              operationQueued.Guardian,
					Operation:             operationQueued.Operation,
					QueueIndex:            operationQueued.QueueIndex,
				})
			}
		}
	})

	eventbus.Publish(bus, AppStartEvent{})

	serviceManager := service.NewManager()

	srv, err := server.NewServer("ZK Certificates Queue Processor", "8080")
	if err != nil {
		panic(err)
	}
	serviceManager.Register(srv)

	evmService, err := evm.NewService("evm", evmRPCURL, bus)
	if err != nil {
		panic(err)
	}
	serviceManager.Register(evmService)

	initBlockHeight, err := certificateRegistry.InitBlockHeight(nil)
	if err != nil {
		slog.Error("Failed to read initBlockHeight from contract", "error", err)
		evmService.RegisterContractFromCurrent(common.HexToAddress(certificateRegistryAddress))
	} else {
		startBlock := new(big.Int).Sub(initBlockHeight, big.NewInt(1))
		evmService.RegisterContract(common.HexToAddress(certificateRegistryAddress), startBlock)
		slog.Info("Registered contract with init block",
			"address", certificateRegistryAddress,
			"initBlockHeight", initBlockHeight.String(),
			"startBlock", startBlock.String())
	}

	queueProcessor, err := queueprocessor.NewService(
		"queue-processor",
		client,
		common.HexToAddress(certificateRegistryAddress),
		bus,
	)
	if err != nil {
		panic(err)
	}

	if privateKey != "" {
		if err := queueProcessor.SetPrivateKey(privateKey); err != nil {
			slog.Error("Failed to set private key", "error", err)
		} else {
			slog.Info("Queue processor configured with private key")
		}
	} else {
		slog.Warn("No PRIVATE_KEY provided, queue processor will run in read-only mode")
	}

	serviceManager.Register(queueProcessor)

	serviceManager.StartAll()

	serviceManager.WaitForShutdown()
}
