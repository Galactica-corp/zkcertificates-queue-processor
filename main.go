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
	"gopkg.in/yaml.v3"
)

type AppStartEvent struct{}

type Config struct {
	Registries []struct {
		Name       string  `yaml:"name"`
		Address    string  `yaml:"address"`
		StartBlock *uint64 `yaml:"startBlock,omitempty"`
	} `yaml:"registries"`
}

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	evmRPCURL := os.Getenv("EVM_RPC_URL")
	if evmRPCURL == "" {
		evmRPCURL = "https://galactica-cassiopeia.g.alchemy.com/public"
		slog.Warn("EVM_RPC_URL not set, using default", "url", evmRPCURL)
	}

	// Load config from YAML file
	configFile := os.Getenv("CONFIG_FILE")
	if configFile == "" {
		configFile = "config.yaml"
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		slog.Error("Failed to read config file", "file", configFile, "error", err)
		os.Exit(1)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		slog.Error("Failed to parse config file", "file", configFile, "error", err)
		os.Exit(1)
	}

	if len(config.Registries) == 0 {
		slog.Error("No registries configured in config file")
		os.Exit(1)
	}

	privateKey := os.Getenv("PRIVATE_KEY")

	// Connect to Ethereum client
	client, err := ethclient.Dial(evmRPCURL)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	bus := eventbus.New()

	// Create a map to store registry contracts by address
	registryContracts := make(map[common.Address]*zkregistry.ZkCertificateRegistry)
	registryNamesByAddress := make(map[common.Address]string)
	registryStartBlocks := make(map[common.Address]*uint64)

	// Load all registry contracts
	for _, registry := range config.Registries {
		address := common.HexToAddress(registry.Address)
		contract, err := zkregistry.NewZkCertificateRegistry(address, client)
		if err != nil {
			slog.Error("Failed to create registry contract", "name", registry.Name, "address", registry.Address, "error", err)
			continue
		}
		registryContracts[address] = contract
		registryNamesByAddress[address] = registry.Name
		registryStartBlocks[address] = registry.StartBlock
		slog.Info("Loaded registry contract", "name", registry.Name, "address", registry.Address)
	}

	// Subscribe to events from all registries
	eventbus.Subscribe(bus, func(event evm.ContractEvent) {
		if registry, ok := registryContracts[event.Contract]; ok {
			registryName := registryNamesByAddress[event.Contract]
			if operationQueued, err := registry.ParseOperationQueued(event.Event); err == nil {
				slog.Info("OperationQueued event",
					"registry", registryName,
					"hash", common.Bytes2Hex(operationQueued.ZkCertificateLeafHash[:]),
					"guardian", operationQueued.Guardian.Hex(),
					"operation", operationQueued.Operation,
					"queueIndex", operationQueued.QueueIndex.String())

				eventbus.Publish(bus, queueprocessor.OperationQueuedEvent{
					RegistryAddress:       event.Contract,
					RegistryName:          registryName,
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

	// Register all registry contracts with the EVM service
	for address, registry := range registryContracts {
		registryName := registryNamesByAddress[address]

		// Check if config provides a start block
		if configStartBlock := registryStartBlocks[address]; configStartBlock != nil {
			startBlock := new(big.Int).SetUint64(*configStartBlock)
			evmService.RegisterContract(address, startBlock)
			slog.Info("Registered contract with config start block",
				"registry", registryName,
				"address", address.Hex(),
				"startBlock", startBlock.String())
		} else {
			// Fall back to reading from contract
			initBlockHeight, err := registry.InitBlockHeight(nil)
			if err != nil {
				slog.Error("Failed to read initBlockHeight from contract", "registry", registryName, "error", err)
				evmService.RegisterContractFromCurrent(address)
			} else {
				startBlock := new(big.Int).Sub(initBlockHeight, big.NewInt(1))
				evmService.RegisterContract(address, startBlock)
				slog.Info("Registered contract with init block",
					"registry", registryName,
					"address", address.Hex(),
					"initBlockHeight", initBlockHeight.String(),
					"startBlock", startBlock.String())
			}
		}
	}

	// Create queue processor with multiple registries
	registries := make([]queueprocessor.RegistryConfig, 0, len(config.Registries))
	for _, reg := range config.Registries {
		registries = append(registries, queueprocessor.RegistryConfig{
			Name:    reg.Name,
			Address: common.HexToAddress(reg.Address),
		})
	}

	queueProcessor, err := queueprocessor.NewServiceWithMultipleRegistries(
		"queue-processor",
		client,
		registries,
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
