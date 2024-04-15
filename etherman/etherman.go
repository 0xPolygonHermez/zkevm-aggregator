package etherman

import (
	"github.com/0xPolygonHermez/zkevm-aggregator/etherman/metrics"
	"github.com/0xPolygonHermez/zkevm-aggregator/etherman/smartcontracts/oldpolygonzkevm"
	"github.com/0xPolygonHermez/zkevm-aggregator/etherman/smartcontracts/polygonrollupmanager"
	"github.com/0xPolygonHermez/zkevm-aggregator/log"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

type ethereumClient interface {
	ethereum.ChainReader
}

// L1Config represents the configuration of the network used in L1
type L1Config struct {
	// Chain ID of the L1 network
	L1ChainID uint64 `json:"chainId"`
	// ZkEVMAddr Address of the L1 contract polygonZkEVMAddress
	ZkEVMAddr common.Address `json:"polygonZkEVMAddress"`
	// RollupManagerAddr Address of the L1 contract
	RollupManagerAddr common.Address `json:"polygonRollupManagerAddress"`
}

// Client is a simple implementation of EtherMan.
type Client struct {
	EthClient     ethereumClient
	OldZkEVM      *oldpolygonzkevm.Oldpolygonzkevm
	RollupManager *polygonrollupmanager.Polygonrollupmanager
	SCAddresses   []common.Address

	RollupID uint32

	l1Cfg L1Config
	cfg   Config
	auth  map[common.Address]bind.TransactOpts // empty in case of read-only client
}

// NewClient creates a new etherman.
func NewClient(cfg Config, l1Config L1Config) (*Client, error) {
	// Connect to ethereum node
	ethClient, err := ethclient.Dial(cfg.URL)
	if err != nil {
		log.Errorf("error connecting to %s: %+v", cfg.URL, err)
		return nil, err
	}
	// Create smc clients
	oldZkevm, err := oldpolygonzkevm.NewOldpolygonzkevm(l1Config.RollupManagerAddr, ethClient)
	if err != nil {
		log.Errorf("error creating NewOldpolygonzkevm client (%s). Error: %w", l1Config.RollupManagerAddr.String(), err)
		return nil, err
	}
	rollupManager, err := polygonrollupmanager.NewPolygonrollupmanager(l1Config.RollupManagerAddr, ethClient)
	if err != nil {
		log.Errorf("error creating NewPolygonrollupmanager client (%s). Error: %w", l1Config.RollupManagerAddr.String(), err)
		return nil, err
	}

	var scAddresses []common.Address
	scAddresses = append(scAddresses, l1Config.ZkEVMAddr, l1Config.RollupManagerAddr)

	metrics.Register()
	// Get RollupID
	rollupID, err := rollupManager.RollupAddressToID(&bind.CallOpts{Pending: false}, l1Config.ZkEVMAddr)
	if err != nil {
		log.Debugf("error rollupManager.RollupAddressToID(%s). Error: %w", l1Config.RollupManagerAddr, err)
		// TODO return error after the upgrade
	}
	log.Debug("rollupID: ", rollupID)

	return &Client{
		EthClient:     ethClient,
		OldZkEVM:      oldZkevm,
		RollupManager: rollupManager,
		SCAddresses:   scAddresses,
		RollupID:      rollupID,
		l1Cfg:         l1Config,
		cfg:           cfg,
		auth:          map[common.Address]bind.TransactOpts{},
	}, nil
}
