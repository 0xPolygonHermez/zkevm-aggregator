package aggregator

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/0xPolygonHermez/zkevm-aggregator/aggregator/metrics"
	"github.com/0xPolygonHermez/zkevm-aggregator/aggregator/prover"
	"github.com/0xPolygonHermez/zkevm-aggregator/config/types"
	ethmanTypes "github.com/0xPolygonHermez/zkevm-aggregator/etherman/types"
	"github.com/0xPolygonHermez/zkevm-aggregator/l1infotree"
	"github.com/0xPolygonHermez/zkevm-aggregator/log"
	"github.com/0xPolygonHermez/zkevm-aggregator/rpclient"
	"github.com/0xPolygonHermez/zkevm-aggregator/state"
	"github.com/0xPolygonHermez/zkevm-data-streamer/datastreamer"
	streamlog "github.com/0xPolygonHermez/zkevm-data-streamer/log"
	"github.com/0xPolygonHermez/zkevm-ethtx-manager/ethtxmanager"
	ethtxlog "github.com/0xPolygonHermez/zkevm-ethtx-manager/log"
	"github.com/0xPolygonHermez/zkevm-synchronizer-l1/synchronizer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/iden3/go-iden3-crypto/keccak256"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
)

const (
	dataStreamType      = 1
	mockedStateRoot     = "0x090bcaf734c4f06c93954a827b45a6e8c67b8e0fd1e0a35a1c5982d6961828f9"
	mockedLocalExitRoot = "0x17c04c3760510b48c6012742c540a81aba4bca2f78b9d14bfd2f123e2e53ea3e"
	// monitoredIDFormat   = "proof-from-%v-to-%v"
)

type finalProofMsg struct {
	proverName     string
	proverID       string
	recursiveProof *state.Proof
	finalProof     *prover.FinalProof
}

// Aggregator represents an aggregator
type Aggregator struct {
	prover.UnimplementedAggregatorServiceServer

	cfg Config

	state        stateInterface
	etherman     etherman
	ethTxManager *ethtxmanager.Client
	streamClient *datastreamer.StreamClient
	l1Syncr      synchronizer.Synchronizer

	profitabilityChecker    aggregatorTxProfitabilityChecker
	timeSendFinalProof      time.Time
	timeCleanupLockedProofs types.Duration
	stateDBMutex            *sync.Mutex
	timeSendFinalProofMutex *sync.RWMutex
	batchAccInputHash       map[uint64]common.Hash
	accInputHashMutes       *sync.RWMutex

	finalProof     chan finalProofMsg
	verifyingProof bool

	srv  *grpc.Server
	ctx  context.Context
	exit context.CancelFunc
}

// New creates a new aggregator.
func New(ctx context.Context, cfg Config, stateInterface stateInterface, etherman etherman) (*Aggregator, error) {
	var profitabilityChecker aggregatorTxProfitabilityChecker

	switch cfg.TxProfitabilityCheckerType {
	case ProfitabilityBase:
		profitabilityChecker = NewTxProfitabilityCheckerBase(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration, cfg.TxProfitabilityMinReward.Int)
	case ProfitabilityAcceptAll:
		profitabilityChecker = NewTxProfitabilityCheckerAcceptAll(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration)
	}

	// Create ethtxmanager client
	cfg.EthTxManager.Log = ethtxlog.Config{
		Environment: ethtxlog.LogEnvironment(cfg.Log.Environment),
		Level:       cfg.Log.Level,
		Outputs:     cfg.Log.Outputs,
	}
	ethTxManager, err := ethtxmanager.New(cfg.EthTxManager)
	if err != nil {
		log.Fatalf("error creating ethtxmanager client: %v", err)
	}

	// Create datastream client (set to error level as it is super verbose by default)
	streamLogConfig := streamlog.Config{
		Environment: streamlog.LogEnvironment(cfg.Log.Environment),
		Level:       "error",
		Outputs:     cfg.Log.Outputs,
	}

	streamClient, err := datastreamer.NewClientWithLogsConfig(cfg.StreamClient.Server, dataStreamType, streamLogConfig)
	if err != nil {
		log.Fatalf("failed to create stream client, error: %v", err)
	}

	// Create L1 synchronizer client
	log.Debugf("Creating synchronizer client with config: %+v", cfg.Synchronizer)
	l1Syncr, err := synchronizer.NewSynchronizer(ctx, cfg.Synchronizer)
	if err != nil {
		log.Fatalf("failed to create synchronizer client, error: %v", err)
	}

	a := &Aggregator{
		cfg:                     cfg,
		state:                   stateInterface,
		etherman:                etherman,
		ethTxManager:            ethTxManager,
		streamClient:            streamClient,
		l1Syncr:                 l1Syncr,
		profitabilityChecker:    profitabilityChecker,
		stateDBMutex:            &sync.Mutex{},
		timeSendFinalProofMutex: &sync.RWMutex{},
		timeCleanupLockedProofs: cfg.CleanupLockedProofsInterval,
		batchAccInputHash:       make(map[uint64]common.Hash),
		accInputHashMutes:       &sync.RWMutex{},

		finalProof: make(chan finalProofMsg),
	}

	return a, nil
}

// Start starts the aggregator
func (a *Aggregator) Start(ctx context.Context) error {
	var cancel context.CancelFunc
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.exit = cancel

	metrics.Register()

	// process monitored batch verifications before starting
	a.ethTxManager.ProcessPendingMonitoredTxs(ctx, func(result ethtxmanager.MonitoredTxResult) {
		a.handleMonitoredTxResult(result)
	})

	// Delete ungenerated recursive proofs
	err := a.state.DeleteUngeneratedProofs(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to initialize proofs cache %w", err)
	}

	address := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	a.srv = grpc.NewServer()
	prover.RegisterAggregatorServiceServer(a.srv, a)

	healthService := newHealthChecker()
	grpchealth.RegisterHealthServer(a.srv, healthService)

	// Initial sync blocking
	err = a.l1Syncr.Sync(true)
	if err != nil {
		return err
	}

	// Initiate AccInputHash map
	lastVerifiedBatchNumber, err := a.etherman.GetLatestVerifiedBatchNum()
	if err != nil {
		return err
	}

	err = a.getAndStoreAccInputHash(ctx, lastVerifiedBatchNumber)
	if err != nil {
		return err
	}

	// Start stream client
	err = a.streamClient.Start()
	if err != nil {
		log.Fatalf("failed to start stream client, error: %v", err)
	}

	// A this point everything is ready, so start serving
	go func() {
		log.Infof("Server listening on port %d", a.cfg.Port)
		if err := a.srv.Serve(lis); err != nil {
			a.exit()
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	a.resetVerifyProofTime()

	go a.cleanupLockedProofs()
	// go a.sendFinalProof()
	go func() {
		err := a.l1Syncr.Sync(false)
		if err != nil {
			log.Errorf("Failed to synchronize from L1: %v", err)
		}
	}()

	<-ctx.Done()
	return ctx.Err()
}

// Stop stops the Aggregator server.
func (a *Aggregator) Stop() {
	a.exit()
	a.srv.Stop()
}

// Channel implements the bi-directional communication channel between the
// Prover client and the Aggregator server.
func (a *Aggregator) Channel(stream prover.AggregatorService_ChannelServer) error {
	metrics.ConnectedProver()
	defer metrics.DisconnectedProver()

	ctx := stream.Context()
	var proverAddr net.Addr
	p, ok := peer.FromContext(ctx)
	if ok {
		proverAddr = p.Addr
	}
	prover, err := prover.New(stream, proverAddr, a.cfg.ProofStatePollingInterval)
	if err != nil {
		return err
	}

	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
	)
	log.Info("Establishing stream connection with prover")

	// Check if prover supports the required Fork ID
	if !prover.SupportsForkID(a.cfg.ForkId) {
		err := errors.New("prover does not support required fork ID")
		log.Warn(FirstToUpper(err.Error()))
		return err
	}

	for {
		select {
		case <-a.ctx.Done():
			// server disconnected
			return a.ctx.Err()
		case <-ctx.Done():
			// client disconnected
			return ctx.Err()

		default:
			isIdle, err := prover.IsIdle()
			if err != nil {
				log.Errorf("Failed to check if prover is idle: %v", err)
				time.Sleep(a.cfg.RetryTime.Duration)
				continue
			}
			if !isIdle {
				log.Debug("Prover is not idle")
				time.Sleep(a.cfg.RetryTime.Duration)
				continue
			}

			_, err = a.tryBuildFinalProof(ctx, prover, nil)
			if err != nil {
				log.Errorf("Error checking proofs to verify: %v", err)
			}

			proofGenerated, err := a.tryAggregateProofs(ctx, prover)
			if err != nil {
				log.Errorf("Error trying to aggregate proofs: %v", err)
			}

			if !proofGenerated {
				proofGenerated, err = a.tryGenerateBatchProof(ctx, prover)
				if err != nil {
					log.Errorf("Error trying to generate proof: %v", err)
				}
			}
			if !proofGenerated {
				// if no proof was generated (aggregated or batch) wait some time before retry
				time.Sleep(a.cfg.RetryTime.Duration)
			} // if proof was generated we retry immediately as probably we have more proofs to process
		}
	}
}

// This function waits to receive a final proof from a prover. Once it receives
// the proof, it performs these steps in order:
// - send the final proof to L1
// - wait for the synchronizer to catch up
// - clean up the cache of recursive proofs
func (a *Aggregator) sendFinalProof() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case msg := <-a.finalProof:
			ctx := a.ctx
			proof := msg.recursiveProof

			log.WithFields("proofId", proof.ProofID, "batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal))
			log.Info("Verifying final proof with ethereum smart contract")

			a.startProofVerification()

			// TODO: Review time.Now()
			_, finalBatch, err := a.getBatchFromDataStream(proof.BatchNumberFinal, time.Now())
			if err != nil {
				log.Errorf("Failed to retrieve batch with number [%d]: %v", proof.BatchNumberFinal, err)
				a.endProofVerification()
				continue
			}

			inputs := ethmanTypes.FinalProofInputs{
				FinalProof: msg.finalProof,
				// NewLocalExitRoot: finalBatch.LocalExitRoot.Bytes(),
				NewStateRoot: finalBatch.StateRoot.Bytes(),
			}

			log.Infof("Final proof inputs: NewLocalExitRoot [%#x], NewStateRoot [%#x]", inputs.NewLocalExitRoot, inputs.NewStateRoot)

			// add batch verification to be monitored
			sender := common.HexToAddress(a.cfg.SenderAddress)
			to, data, err := a.etherman.BuildTrustedVerifyBatchesTxData(proof.BatchNumber-1, proof.BatchNumberFinal, &inputs, sender)
			if err != nil {
				log.Errorf("Error estimating batch verification to add to eth tx manager: %v", err)
				a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
				continue
			}
			monitoredTxID, err := a.ethTxManager.Add(ctx, to, nil, big.NewInt(0), data)
			if err != nil {
				mTxLogger := ethtxmanager.CreateLogger(monitoredTxID, sender, to)
				mTxLogger.Errorf("Error to add batch verification tx to eth tx manager: %v", err)
				a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
				continue
			}

			// process monitored batch verifications before starting a next cycle
			a.ethTxManager.ProcessPendingMonitoredTxs(ctx, func(result ethtxmanager.MonitoredTxResult) {
				a.handleMonitoredTxResult(result)
			})

			a.resetVerifyProofTime()
			a.endProofVerification()
		}
	}
}

func (a *Aggregator) handleFailureToAddVerifyBatchToBeMonitored(ctx context.Context, proof *state.Proof) {
	log := log.WithFields("proofId", proof.ProofID, "batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal))
	proof.GeneratingSince = nil
	err := a.state.UpdateGeneratedProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed updating proof state (false): %v", err)
	}
	a.endProofVerification()
}

// buildFinalProof builds and return the final proof for an aggregated/batch proof.
func (a *Aggregator) buildFinalProof(ctx context.Context, prover proverInterface, proof *state.Proof) (*prover.FinalProof, error) {
	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
		"recursiveProofId", *proof.ProofID,
		"batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal),
	)
	log.Info("Generating final proof")

	finalProofID, err := prover.FinalProof(proof.Proof, a.cfg.SenderAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get final proof id: %w", err)
	}
	proof.ProofID = finalProofID

	log.Infof("Final proof ID for batches [%d-%d]: %s", proof.BatchNumber, proof.BatchNumberFinal, *proof.ProofID)
	log = log.WithFields("finalProofId", finalProofID)

	finalProof, err := prover.WaitFinalProof(ctx, *proof.ProofID)
	if err != nil {
		return nil, fmt.Errorf("failed to get final proof from prover: %w", err)
	}

	log.Info("Final proof generated")

	// mock prover sanity check
	if string(finalProof.Public.NewStateRoot) == mockedStateRoot && string(finalProof.Public.NewLocalExitRoot) == mockedLocalExitRoot {
		// This local exit root and state root come from the mock
		// prover, use the one captured by the executor instead

		// TODO: Review time.Now() does not break anything
		_, finalBatch, err := a.getBatchFromDataStream(proof.BatchNumberFinal, time.Now())
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve batch with number [%d]", proof.BatchNumberFinal)
		}
		log.Warnf("NewLocalExitRoot and NewStateRoot look like a mock values, using values from executor instead: LER: %v, SR: %v",
			finalBatch.LocalExitRoot.TerminalString(), finalBatch.StateRoot.TerminalString())
		finalProof.Public.NewStateRoot = finalBatch.StateRoot.Bytes()
		finalProof.Public.NewLocalExitRoot = finalBatch.LocalExitRoot.Bytes()
	}

	return finalProof, nil
}

// tryBuildFinalProof checks if the provided proof is eligible to be used to
// build the final proof.  If no proof is provided it looks for a previously
// generated proof.  If the proof is eligible, then the final proof generation
// is triggered.
func (a *Aggregator) tryBuildFinalProof(ctx context.Context, prover proverInterface, proof *state.Proof) (bool, error) {
	proverName := prover.Name()
	proverID := prover.ID()

	log := log.WithFields(
		"prover", proverName,
		"proverId", proverID,
		"proverAddr", prover.Addr(),
	)
	log.Debug("tryBuildFinalProof start")

	var err error
	if !a.canVerifyProof() {
		log.Debug("Time to verify proof not reached or proof verification in progress")
		return false, nil
	}
	log.Debug("Send final proof time reached")

	lastVerifiedBatchNumber, err := a.etherman.GetLatestVerifiedBatchNum()
	if err != nil {
		return false, err
	}

	if proof == nil {
		// we don't have a proof generating at the moment, check if we
		// have a proof ready to verify

		proof, err = a.getAndLockProofReadyToVerify(ctx, prover, lastVerifiedBatchNumber)
		if errors.Is(err, state.ErrNotFound) {
			// nothing to verify, swallow the error
			log.Debug("No proof ready to verify")
			return false, nil
		}
		if err != nil {
			return false, err
		}

		defer func() {
			if err != nil {
				// Set the generating state to false for the proof ("unlock" it)
				proof.GeneratingSince = nil
				err2 := a.state.UpdateGeneratedProof(a.ctx, proof, nil)
				if err2 != nil {
					log.Errorf("Failed to unlock proof: %v", err2)
				}
			}
		}()
	} else {
		// we do have a proof generating at the moment, check if it is
		// eligible to be verified
		eligible, err := a.validateEligibleFinalProof(ctx, proof, lastVerifiedBatchNumber)
		if err != nil {
			return false, fmt.Errorf("failed to validate eligible final proof, %w", err)
		}
		if !eligible {
			return false, nil
		}
	}

	log = log.WithFields(
		"proofId", *proof.ProofID,
		"batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal),
	)

	// at this point we have an eligible proof, build the final one using it
	finalProof, err := a.buildFinalProof(ctx, prover, proof)
	if err != nil {
		err = fmt.Errorf("failed to build final proof, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	msg := finalProofMsg{
		proverName:     proverName,
		proverID:       proverID,
		recursiveProof: proof,
		finalProof:     finalProof,
	}

	select {
	case <-a.ctx.Done():
		return false, a.ctx.Err()
	case a.finalProof <- msg:
	}

	log.Debug("tryBuildFinalProof end")
	return true, nil
}

func (a *Aggregator) validateEligibleFinalProof(ctx context.Context, proof *state.Proof, lastVerifiedBatchNum uint64) (bool, error) {
	batchNumberToVerify := lastVerifiedBatchNum + 1

	if proof.BatchNumber != batchNumberToVerify {
		if proof.BatchNumber < batchNumberToVerify && proof.BatchNumberFinal >= batchNumberToVerify {
			// We have a proof that contains some batches below the last batch verified, anyway can be eligible as final proof
			log.Warnf("Proof %d-%d contains some batches lower than last batch verified %d. Check anyway if it is eligible", proof.BatchNumber, proof.BatchNumberFinal, lastVerifiedBatchNum)
		} else if proof.BatchNumberFinal < batchNumberToVerify {
			// We have a proof that contains batches below that the last batch verified, we need to delete this proof
			log.Warnf("Proof %d-%d lower than next batch to verify %d. Deleting it", proof.BatchNumber, proof.BatchNumberFinal, batchNumberToVerify)
			err := a.state.DeleteGeneratedProofs(ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err != nil {
				return false, fmt.Errorf("failed to delete discarded proof, err: %w", err)
			}
			return false, nil
		} else {
			log.Debugf("Proof batch number %d is not the following to last verfied batch number %d", proof.BatchNumber, lastVerifiedBatchNum)
			return false, nil
		}
	}

	bComplete, err := a.state.CheckProofContainsCompleteSequences(ctx, proof, nil)
	if err != nil {
		return false, fmt.Errorf("failed to check if proof contains complete sequences, %w", err)
	}
	if !bComplete {
		log.Infof("Recursive proof %d-%d not eligible to be verified: not containing complete sequences", proof.BatchNumber, proof.BatchNumberFinal)
		return false, nil
	}
	return true, nil
}

func (a *Aggregator) getAndLockProofReadyToVerify(ctx context.Context, prover proverInterface, lastVerifiedBatchNum uint64) (*state.Proof, error) {
	a.stateDBMutex.Lock()
	defer a.stateDBMutex.Unlock()

	// Get proof ready to be verified
	proofToVerify, err := a.state.GetProofReadyToVerify(ctx, lastVerifiedBatchNum, nil)
	if err != nil {
		return nil, err
	}

	now := time.Now().Round(time.Microsecond)
	proofToVerify.GeneratingSince = &now

	err = a.state.UpdateGeneratedProof(ctx, proofToVerify, nil)
	if err != nil {
		return nil, err
	}

	return proofToVerify, nil
}

func (a *Aggregator) unlockProofsToAggregate(ctx context.Context, proof1 *state.Proof, proof2 *state.Proof) error {
	// Release proofs from generating state in a single transaction
	dbTx, err := a.state.BeginStateTransaction(ctx)
	if err != nil {
		log.Warnf("Failed to begin transaction to release proof aggregation state, err: %v", err)
		return err
	}

	proof1.GeneratingSince = nil
	err = a.state.UpdateGeneratedProof(ctx, proof1, dbTx)
	if err == nil {
		proof2.GeneratingSince = nil
		err = a.state.UpdateGeneratedProof(ctx, proof2, dbTx)
	}

	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state: %w", err)
			log.Error(FirstToUpper(err.Error()))
			return err
		}
		return fmt.Errorf("failed to release proof aggregation state: %w", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("failed to release proof aggregation state %w", err)
	}

	return nil
}

func (a *Aggregator) getAndLockProofsToAggregate(ctx context.Context, prover proverInterface) (*state.Proof, *state.Proof, error) {
	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
	)

	a.stateDBMutex.Lock()
	defer a.stateDBMutex.Unlock()

	proof1, proof2, err := a.state.GetProofsToAggregate(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	// Set proofs in generating state in a single transaction
	dbTx, err := a.state.BeginStateTransaction(ctx)
	if err != nil {
		log.Errorf("Failed to begin transaction to set proof aggregation state, err: %v", err)
		return nil, nil, err
	}

	now := time.Now().Round(time.Microsecond)
	proof1.GeneratingSince = &now
	err = a.state.UpdateGeneratedProof(ctx, proof1, dbTx)
	if err == nil {
		proof2.GeneratingSince = &now
		err = a.state.UpdateGeneratedProof(ctx, proof2, dbTx)
	}

	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state %w", err)
			log.Error(FirstToUpper(err.Error()))
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("failed to set proof aggregation state %w", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set proof aggregation state %w", err)
	}

	return proof1, proof2, nil
}

func (a *Aggregator) tryAggregateProofs(ctx context.Context, prover proverInterface) (bool, error) {
	proverName := prover.Name()
	proverID := prover.ID()

	log := log.WithFields(
		"prover", proverName,
		"proverId", proverID,
		"proverAddr", prover.Addr(),
	)
	log.Debug("tryAggregateProofs start")

	proof1, proof2, err0 := a.getAndLockProofsToAggregate(ctx, prover)
	if errors.Is(err0, state.ErrNotFound) {
		// nothing to aggregate, swallow the error
		log.Debug("Nothing to aggregate")
		return false, nil
	}
	if err0 != nil {
		return false, err0
	}

	var (
		aggrProofID *string
		err         error
	)

	defer func() {
		if err != nil {
			err2 := a.unlockProofsToAggregate(a.ctx, proof1, proof2)
			if err2 != nil {
				log.Errorf("Failed to release aggregated proofs, err: %v", err2)
			}
		}
		log.Debug("tryAggregateProofs end")
	}()

	log.Infof("Aggregating proofs: %d-%d and %d-%d", proof1.BatchNumber, proof1.BatchNumberFinal, proof2.BatchNumber, proof2.BatchNumberFinal)

	batches := fmt.Sprintf("%d-%d", proof1.BatchNumber, proof2.BatchNumberFinal)
	log = log.WithFields("batches", batches)

	inputProver := map[string]interface{}{
		"recursive_proof_1": proof1.Proof,
		"recursive_proof_2": proof2.Proof,
	}
	b, err := json.Marshal(inputProver)
	if err != nil {
		err = fmt.Errorf("failed to serialize input prover, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof := &state.Proof{
		BatchNumber:      proof1.BatchNumber,
		BatchNumberFinal: proof2.BatchNumberFinal,
		Prover:           &proverName,
		ProverID:         &proverID,
		InputProver:      string(b),
	}

	aggrProofID, err = prover.AggregatedProof(proof1.Proof, proof2.Proof)
	if err != nil {
		err = fmt.Errorf("failed to get aggregated proof id, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof.ProofID = aggrProofID

	log.Infof("Proof ID for aggregated proof: %v", *proof.ProofID)
	log = log.WithFields("proofId", *proof.ProofID)

	recursiveProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
	if err != nil {
		err = fmt.Errorf("failed to get aggregated proof from prover, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	log.Info("Aggregated proof generated")

	proof.Proof = recursiveProof

	// update the state by removing the 2 aggregated proofs and storing the
	// newly generated recursive proof
	dbTx, err := a.state.BeginStateTransaction(ctx)
	if err != nil {
		err = fmt.Errorf("failed to begin transaction to update proof aggregation state, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	err = a.state.DeleteGeneratedProofs(ctx, proof1.BatchNumber, proof2.BatchNumberFinal, dbTx)
	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state, %w", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
		err = fmt.Errorf("failed to delete previously aggregated proofs, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	now := time.Now().Round(time.Microsecond)
	proof.GeneratingSince = &now

	err = a.state.AddGeneratedProof(ctx, proof, dbTx)
	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state, %w", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
		err = fmt.Errorf("failed to store the recursive proof, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		err = fmt.Errorf("failed to store the recursive proof, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	// NOTE(pg): the defer func is useless from now on, use a different variable
	// name for errors (or shadow err in inner scopes) to not trigger it.

	// state is up to date, check if we can send the final proof using the
	// one just crafted.
	finalProofBuilt, finalProofErr := a.tryBuildFinalProof(ctx, prover, proof)
	if finalProofErr != nil {
		// just log the error and continue to handle the aggregated proof
		log.Errorf("Failed trying to check if recursive proof can be verified: %v", finalProofErr)
	}

	// NOTE(pg): prover is done, use a.ctx from now on

	if !finalProofBuilt {
		proof.GeneratingSince = nil

		// final proof has not been generated, update the recursive proof
		err := a.state.UpdateGeneratedProof(a.ctx, proof, nil)
		if err != nil {
			err = fmt.Errorf("failed to store batch proof result, %w", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
	}

	return true, nil
}

func (a *Aggregator) getBatchFromDataStream(batchNumber uint64, bactchTimestamp time.Time) ([]byte, *state.Batch, error) {
	var batchStreamData []byte

	fromBatchBookMark := state.DSBookMark{
		Type:  state.BookMarkTypeBatch,
		Value: batchNumber,
	}

	toBatchBookMark := state.DSBookMark{
		Type:  state.BookMarkTypeBatch,
		Value: batchNumber + 1,
	}

	fromEntry, err := a.streamClient.ExecCommandGetBookmark(fromBatchBookMark.Encode())
	if err != nil {
		return nil, nil, err
	}

	log.Infof("fromBatchBookMark %d, found", fromBatchBookMark.Value)

	toEntry, err := a.streamClient.ExecCommandGetBookmark(toBatchBookMark.Encode())
	if err != nil {
		return nil, nil, err
	}

	log.Infof("toBatchBookMark %d, found", toBatchBookMark.Value)

	l2BlockStart := state.DSL2BlockStart{}.Decode(fromEntry.Data)
	batch := state.Batch{
		L1InfoTreeIndex: l2BlockStart.L1InfoTreeIndex,
		BatchNumber:     batchNumber,
		Coinbase:        l2BlockStart.Coinbase,
		GlobalExitRoot:  l2BlockStart.GlobalExitRoot,
		Timestamp:       bactchTimestamp,
		// LocalExitRoot:  l2BlockStart.LocalExitRoot,
	}

	batchRaw := state.BatchRawV2{
		Blocks: make([]state.L2BlockRaw, 0),
	}
	var currentL2Block state.L2BlockRaw

	for fromEntry.Number < toEntry.Number {
		batchStreamData = append(batchStreamData, fromEntry.Encode()...)
		entry, err := a.streamClient.ExecCommandGetEntry(fromEntry.Number + 1)
		if err != nil {
			return nil, nil, err
		}

		switch entry.Type {
		case state.EntryTypeL2BlockStart:
			l2BlockStart := state.DSL2BlockStart{}.Decode(entry.Data)
			header := state.ChangeL2BlockHeader{
				DeltaTimestamp:  l2BlockStart.DeltaTimestamp,
				IndexL1InfoTree: l2BlockStart.L1InfoTreeIndex,
			}
			currentL2Block.ChangeL2BlockHeader = header
			currentL2Block.Transactions = make([]state.L2TxRaw, 0)
			batch.L1InfoTreeIndex = l2BlockStart.L1InfoTreeIndex
			batch.GlobalExitRoot = l2BlockStart.GlobalExitRoot
		case state.EntryTypeL2Tx:
			l2Tx := state.DSL2Transaction{}.Decode(entry.Data)
			// New Tx raw
			tx, err := state.DecodeTx(common.Bytes2Hex(l2Tx.Encoded))
			if err != nil {
				return nil, nil, err
			}

			l2TxRaw := state.L2TxRaw{
				EfficiencyPercentage: l2Tx.EffectiveGasPricePercentage,
				TxAlreadyEncoded:     false,
				Tx:                   *tx,
			}
			currentL2Block.Transactions = append(currentL2Block.Transactions, l2TxRaw)
		case state.EntryTypeL2BlockEnd:
			l2BlockEnd := state.DSL2BlockEnd{}.Decode(entry.Data)
			batchRaw.Blocks = append(batchRaw.Blocks, currentL2Block)
			batch.StateRoot = l2BlockEnd.StateRoot
		}

		fromEntry = entry
	}

	batchl2Data, err := state.EncodeBatchV2(&batchRaw)
	if err != nil {
		return nil, nil, err
	}

	batch.BatchL2Data = batchl2Data

	return batchStreamData, &batch, nil
}

func (a *Aggregator) getAndStoreAccInputHash(ctx context.Context, batchNumber uint64) error {
	accInputHash, err := a.etherman.GetBatchAccInputHash(ctx, batchNumber)
	if err != nil {
		return err
	}

	log.Debugf("AccInputHash for batch %v: %v", batchNumber, accInputHash.String())

	a.accInputHashMutes.Lock()
	a.batchAccInputHash[batchNumber] = accInputHash
	a.accInputHashMutes.Unlock()

	return nil
}

func (a *Aggregator) getAndLockBatchToProve(ctx context.Context, prover proverInterface) ([]byte, *state.Batch, *state.Proof, error) {
	proverID := prover.ID()
	proverName := prover.Name()

	log := log.WithFields(
		"prover", proverName,
		"proverId", proverID,
		"proverAddr", prover.Addr(),
	)

	a.stateDBMutex.Lock()
	defer a.stateDBMutex.Unlock()

	// Get last virtual batch number from L1
	lastVerifiedBatchNumber, err := a.etherman.GetLatestVerifiedBatchNum()
	if err != nil {
		return nil, nil, nil, err
	}

	batchNumberToVerify := lastVerifiedBatchNumber + 1
	log.Debugf("Batch Number To Verify: %d", batchNumberToVerify)

	// Check if the batch exists
	sequence, err := a.l1Syncr.GetSequenceByBatchNumber(ctx, batchNumberToVerify)
	if err != nil {
		return nil, nil, nil, err
	}

	// Not found, so it it not possible to verify the batch yet
	if sequence == nil {
		return nil, nil, nil, state.ErrNotFound
	}

	// Get virtual batch pending to generate proof from the data stream
	batchStreamData, batch, err := a.getBatchFromDataStream(batchNumberToVerify, sequence.Timestamp)
	if err != nil {
		return nil, batch, nil, err
	}

	log.Infof("Found virtual batch %d pending to generate proof", batch.BatchNumber)
	log = log.WithFields("batch", batch.BatchNumber)

	log.Info("Checking profitability to aggregate batch")

	// pass pol collateral as zero here, bcs in smart contract fee for aggregator is not defined yet
	isProfitable, err := a.profitabilityChecker.IsProfitable(ctx, big.NewInt(0))
	if err != nil {
		log.Errorf("Failed to check aggregator profitability, err: %v", err)
		return nil, nil, nil, err
	}

	if !isProfitable {
		log.Infof("Batch is not profitable, pol collateral %d", big.NewInt(0))
		return nil, nil, nil, err
	}

	now := time.Now().Round(time.Microsecond)
	proof := &state.Proof{
		BatchNumber:             batch.BatchNumber,
		BatchNumberFinal:        batch.BatchNumber,
		SequenceFromBatchNumber: sequence.FromBatchNumber,
		SequenceToBatchNumber:   sequence.ToBatchNumber,
		Prover:                  &proverName,
		ProverID:                &proverID,
		GeneratingSince:         &now,
	}

	// Avoid other prover to process the same batch
	err = a.state.AddGeneratedProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed to add batch proof, err: %v", err)
		return nil, nil, nil, err
	}

	return batchStreamData, batch, proof, nil
}

func (a *Aggregator) tryGenerateBatchProof(ctx context.Context, prover proverInterface) (bool, error) {
	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
	)
	log.Debug("tryGenerateBatchProof start")

	batchDataToProve, batchToProve, proof, err0 := a.getAndLockBatchToProve(ctx, prover)
	if errors.Is(err0, state.ErrNotFound) {
		// nothing to proof, swallow the error
		log.Debug("Nothing to generate proof")
		return false, nil
	}
	if err0 != nil {
		return false, err0
	}

	log = log.WithFields("batch", batchToProve.BatchNumber)

	var (
		genProofID *string
		err        error
	)

	defer func() {
		if err != nil {
			err2 := a.state.DeleteGeneratedProofs(a.ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err2 != nil {
				log.Errorf("Failed to delete proof in progress, err: %v", err2)
			}
		}
		log.Debug("tryGenerateBatchProof end")
	}()

	log.Info("Generating proof from batch")

	log.Infof("Sending zki + batch to the prover, batchNumber [%d]", batchToProve.BatchNumber)
	inputProver, err := a.buildInputProver(ctx, batchDataToProve, batchToProve)
	if err != nil {
		err = fmt.Errorf("failed to build input prover, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	b, err := json.Marshal(inputProver)
	if err != nil {
		err = fmt.Errorf("failed to serialize input prover, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof.InputProver = string(b)

	log.Infof("Sending a batch to the prover. OldAccInputHash [%#x], L1InfoRoot [%#x]",
		inputProver.PublicInputs.OldAccInputHash, inputProver.PublicInputs.L1InfoRoot)

	genProofID, err = prover.BatchProof(inputProver)
	if err != nil {
		err = fmt.Errorf("failed to get batch proof id, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof.ProofID = genProofID

	log.Infof("Proof ID %v", *proof.ProofID)
	log = log.WithFields("proofId", *proof.ProofID)

	resGetProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
	if err != nil {
		err = fmt.Errorf("failed to get proof from prover, %w", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	log.Info("Batch proof generated")

	proof.Proof = resGetProof

	// NOTE(pg): the defer func is useless from now on, use a different variable
	// name for errors (or shadow err in inner scopes) to not trigger it.

	finalProofBuilt, finalProofErr := a.tryBuildFinalProof(ctx, prover, proof)
	if finalProofErr != nil {
		// just log the error and continue to handle the generated proof
		log.Errorf("Error trying to build final proof: %v", finalProofErr)
	}

	// NOTE(pg): prover is done, use a.ctx from now on

	if !finalProofBuilt {
		proof.GeneratingSince = nil

		// final proof has not been generated, update the batch proof
		err := a.state.UpdateGeneratedProof(a.ctx, proof, nil)
		if err != nil {
			err = fmt.Errorf("failed to store batch proof result, %w", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
	}

	return true, nil
}

// canVerifyProof returns true if we have reached the timeout to verify a proof
// and no other prover is verifying a proof (verifyingProof = false).
func (a *Aggregator) canVerifyProof() bool {
	a.timeSendFinalProofMutex.RLock()
	defer a.timeSendFinalProofMutex.RUnlock()
	return a.timeSendFinalProof.Before(time.Now()) && !a.verifyingProof
}

// startProofVerification sets to true the verifyingProof variable to indicate that there is a proof verification in progress
func (a *Aggregator) startProofVerification() {
	a.timeSendFinalProofMutex.Lock()
	defer a.timeSendFinalProofMutex.Unlock()
	a.verifyingProof = true
}

// endProofVerification set verifyingProof to false to indicate that there is not proof verification in progress
func (a *Aggregator) endProofVerification() {
	a.timeSendFinalProofMutex.Lock()
	defer a.timeSendFinalProofMutex.Unlock()
	a.verifyingProof = false
}

// resetVerifyProofTime updates the timeout to verify a proof.
func (a *Aggregator) resetVerifyProofTime() {
	a.timeSendFinalProofMutex.Lock()
	defer a.timeSendFinalProofMutex.Unlock()
	a.timeSendFinalProof = time.Now().Add(a.cfg.VerifyProofInterval.Duration)
}

func (a *Aggregator) buildInputProver(ctx context.Context, batchStreamData []byte, batchToVerify *state.Batch) (*prover.StatelessInputProver, error) {
	sequence, err := a.l1Syncr.GetSequenceByBatchNumber(ctx, batchToVerify.BatchNumber)
	if err != nil {
		return nil, err
	}

	if sequence == nil {
		return nil, fmt.Errorf("sequence not found for batch number %v", batchToVerify.BatchNumber)
	}
	l1InfoRoot := sequence.L1InfoRoot

	isForcedBatch := false
	batchRawData := &state.BatchRawV2{}

	if batchToVerify.BatchNumber == 1 || batchToVerify.ForcedBatchNum != nil || batchToVerify.BatchNumber == a.cfg.UpgradeEtrogBatchNumber {
		isForcedBatch = true
	} else {
		batchRawData, err = state.DecodeBatchV2(batchToVerify.BatchL2Data)
		if err != nil {
			log.Errorf("Failed to decode batch data, err: %v", err)
			return nil, err
		}
	}

	l1InfoTreeData := map[uint32]*prover.L1Data{}
	forcedBlockhashL1 := common.Hash{}
	if !isForcedBatch {
		tree, err := l1infotree.NewL1InfoTree(32, [][32]byte{}) // nolint:gomnd
		if err != nil {
			return nil, err
		}

		leaves, err := a.l1Syncr.GetLeafsByL1InfoRoot(ctx, l1InfoRoot, nil)
		if err != nil {
			return nil, err
		}

		aLeaves := make([][32]byte, len(leaves))
		for i, leaf := range leaves {
			aLeaves[i] = l1infotree.HashLeafData(leaf.GlobalExitRoot, leaf.PreviousBlockHash, uint64(leaf.Timestamp.Unix()))
		}

		for _, l2blockRaw := range batchRawData.Blocks {
			_, contained := l1InfoTreeData[l2blockRaw.IndexL1InfoTree]
			if !contained && l2blockRaw.IndexL1InfoTree != 0 {
				leaves, err := a.l1Syncr.GetL1InfoTreeLeaves(ctx, []uint32{l2blockRaw.IndexL1InfoTree})
				if err != nil {
					return nil, err
				}

				l1InfoTreeLeaf := leaves[l2blockRaw.IndexL1InfoTree]

				// Calculate smt proof
				log.Infof("Calling tree.ComputeMerkleProof")
				smtProof, calculatedL1InfoRoot, err := tree.ComputeMerkleProof(l2blockRaw.IndexL1InfoTree, aLeaves)
				if err != nil {
					return nil, err
				}

				if l1InfoRoot != calculatedL1InfoRoot {
					for i, l := range aLeaves {
						log.Info("AllLeaves[%d]: %s", i, common.Bytes2Hex(l[:]))
					}
					for i, s := range smtProof {
						log.Info("smtProof[%d]: %s", i, common.Bytes2Hex(s[:]))
					}
					return nil, fmt.Errorf("error: l1InfoRoot mismatch. L1InfoRoot: %s, calculatedL1InfoRoot: %s. l1InfoTreeIndex: %d", l1InfoRoot.String(), calculatedL1InfoRoot.String(), l2blockRaw.IndexL1InfoTree)
				}

				protoProof := make([][]byte, len(smtProof))
				for i, proof := range smtProof {
					tmpProof := proof
					protoProof[i] = tmpProof[:]
				}

				l1InfoTreeData[l2blockRaw.IndexL1InfoTree] = &prover.L1Data{
					GlobalExitRoot: l1InfoTreeLeaf.GlobalExitRoot.Bytes(),
					BlockhashL1:    l1InfoTreeLeaf.PreviousBlockHash.Bytes(),
					MinTimestamp:   uint32(l1InfoTreeLeaf.Timestamp.Unix()),
					SmtProof:       protoProof,
				}
			}
		}
	} /*else {
		// Initial batch must be handled differently
		if batchToVerify.BatchNumber == 1 || batchToVerify.BatchNumber == a.cfg.UpgradeEtrogBatchNumber {
			forcedBlockhashL1, err = a.state.GetVirtualBatchParentHash(ctx, batchToVerify.BatchNumber, nil)
			if err != nil {
				return nil, err
			}
		} else {
			forcedBlockhashL1, err = a.state.GetForcedBatchParentHash(ctx, *batchToVerify.ForcedBatchNum, nil)
			if err != nil {
				return nil, err
			}
		}
	}*/

	// Get Witness
	witness, err := getWitness(batchToVerify.BatchNumber, a.cfg.WitnessURL)
	if err != nil {
		return nil, err
	}

	log.Info("Witness generated.")

	// TODO: Improve logic to allow concurrency and set as pre requisite much earlier
	// Calculate accInputHash
	a.accInputHashMutes.RLock()
	oldAccInputHash := a.batchAccInputHash[batchToVerify.BatchNumber-1]
	a.accInputHashMutes.RUnlock()

	// TODO: Store accInputHash along the proof in order to use it in case of restart
	if oldAccInputHash == state.ZeroHash {
		return nil, fmt.Errorf("failed to get oldAccInputHash for batch %d", batchToVerify.BatchNumber)
	}

	accInputHash, err := calculateAccInputHash(oldAccInputHash, batchToVerify.BatchL2Data, l1InfoRoot, uint64(batchToVerify.Timestamp.Unix()), batchToVerify.Coinbase, forcedBlockhashL1)
	if err != nil {
		return nil, err
	}

	log.Infof("Calculated Acc Input Hash: %v", oldAccInputHash)

	// Store new accInputHash
	a.accInputHashMutes.Lock()
	a.batchAccInputHash[batchToVerify.BatchNumber] = accInputHash
	a.accInputHashMutes.Unlock()

	inputProver := &prover.StatelessInputProver{
		PublicInputs: &prover.StatelessPublicInputs{
			Witness:           witness,
			DataStream:        batchStreamData,
			OldAccInputHash:   oldAccInputHash.Bytes(),
			L1InfoRoot:        l1InfoRoot.Bytes(),
			TimestampLimit:    uint64(batchToVerify.Timestamp.Unix()),
			SequencerAddr:     batchToVerify.Coinbase.String(),
			AggregatorAddr:    a.cfg.SenderAddress,
			L1InfoTreeData:    l1InfoTreeData,
			ForcedBlockhashL1: common.Hash{}.Bytes(),
		},
	}

	printInputProver(inputProver)

	r := reflect.ValueOf(inputProver)
	s := binary.Size(r)

	log.Infof("TotalSize = %v bytes", s)
	log.Infof("Witness: %v", len(witness))
	log.Infof("DataStream: %v", len(batchStreamData))
	log.Infof("L1InfoTreeData: %v", len(l1InfoTreeData))

	return inputProver, nil
}

func calculateAccInputHash(oldAccInputHash common.Hash, batchHashData []byte, l1InfoRoot common.Hash, timestampLimit uint64, sequencerAddr common.Address, forcedBlockhashL1 common.Hash) (common.Hash, error) {
	v1 := oldAccInputHash.Bytes()
	v2 := batchHashData
	v3 := l1InfoRoot.Bytes()
	v4 := big.NewInt(0).SetUint64(timestampLimit).Bytes()
	v5 := sequencerAddr.Bytes()
	v6 := forcedBlockhashL1.Bytes()

	// Add 0s to make values 32 bytes long
	for len(v1) < 32 {
		v1 = append([]byte{0}, v1...)
	}
	for len(v2) < 32 {
		v2 = append([]byte{0}, v2...)
	}
	for len(v3) < 32 {
		v3 = append([]byte{0}, v3...)
	}
	for len(v4) < 32 {
		v4 = append([]byte{0}, v4...)
	}
	for len(v5) < 32 {
		v5 = append([]byte{0}, v5...)
	}
	for len(v6) < 32 {
		v6 = append([]byte{0}, v6...)
	}

	return common.BytesToHash(keccak256.Hash(v1, v2, v3, v4, v5, v6)), nil
}

func getWitness(batchNumber uint64, URL string) ([]byte, error) {
	var witness string
	response, err := rpclient.JSONRPCCall(URL, "zkevm_getBatchWitness", nil, "batch-1", batchNumber)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(response.Result, &witness)
	if err != nil {
		return nil, err
	}

	witnessString := strings.TrimLeft(witness, "0x")
	if len(witnessString)%2 != 0 {
		witnessString = "0" + witnessString
	}
	bytes := common.Hex2Bytes(witnessString)

	return bytes, nil
}

func printInputProver(inputProver *prover.StatelessInputProver) {
	log.Debugf("Witness length: %v", len(inputProver.PublicInputs.Witness))
	log.Debugf("DataStream length: %v", len(inputProver.PublicInputs.DataStream))
	// log.Debugf("Full DataStream: %v", common.Bytes2Hex(inputProver.PublicInputs.DataStream))
	log.Debugf("OldAccInputHash: %v", common.BytesToHash(inputProver.PublicInputs.OldAccInputHash))
	log.Debugf("L1InfoRoot: %v", common.BytesToHash(inputProver.PublicInputs.L1InfoRoot))
	log.Debugf("TimestampLimit: %v", inputProver.PublicInputs.TimestampLimit)
	log.Debugf("SequencerAddr: %v", inputProver.PublicInputs.SequencerAddr)
	log.Debugf("AggregatorAddr: %v", inputProver.PublicInputs.AggregatorAddr)
	log.Debugf("L1InfoTreeData: %+v", inputProver.PublicInputs.L1InfoTreeData)
	log.Debugf("ForcedBlockhashL1: %v", common.Bytes2Hex(inputProver.PublicInputs.ForcedBlockhashL1))
}

// healthChecker will provide an implementation of the HealthCheck interface.
type healthChecker struct{}

// newHealthChecker returns a health checker according to standard package
// grpc.health.v1.
func newHealthChecker() *healthChecker {
	return &healthChecker{}
}

// HealthCheck interface implementation.

// Check returns the current status of the server for unary gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Check(ctx context.Context, req *grpchealth.HealthCheckRequest) (*grpchealth.HealthCheckResponse, error) {
	log.Info("Serving the Check request for health check")
	return &grpchealth.HealthCheckResponse{
		Status: grpchealth.HealthCheckResponse_SERVING,
	}, nil
}

// Watch returns the current status of the server for stream gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Watch(req *grpchealth.HealthCheckRequest, server grpchealth.Health_WatchServer) error {
	log.Info("Serving the Watch request for health check")
	return server.Send(&grpchealth.HealthCheckResponse{
		Status: grpchealth.HealthCheckResponse_SERVING,
	})
}

func (a *Aggregator) handleMonitoredTxResult(result ethtxmanager.MonitoredTxResult) {
	mTxResultLogger := ethtxmanager.CreateMonitoredTxResultLogger(result)
	if result.Status == ethtxmanager.MonitoredTxStatusFailed {
		mTxResultLogger.Fatal("failed to send batch verification, TODO: review this fatal and define what to do in this case")
	}

	// TODO: REVIEW THIS

	/*
	   // monitoredIDFormat: "proof-from-%v-to-%v"
	   idSlice := strings.Split(result.ID, "-")
	   proofBatchNumberStr := idSlice[2]
	   proofBatchNumber, err := strconv.ParseUint(proofBatchNumberStr, encoding.Base10, 0)

	   	if err != nil {
	   		mTxResultLogger.Errorf("failed to read final proof batch number from monitored tx: %v", err)
	   	}

	   proofBatchNumberFinalStr := idSlice[4]
	   proofBatchNumberFinal, err := strconv.ParseUint(proofBatchNumberFinalStr, encoding.Base10, 0)

	   	if err != nil {
	   		mTxResultLogger.Errorf("failed to read final proof batch number final from monitored tx: %v", err)
	   	}

	   log := log.WithFields("txId", result.ID, "batches", fmt.Sprintf("%d-%d", proofBatchNumber, proofBatchNumberFinal))
	   log.Info("Final proof verified")

	   // wait for the synchronizer to catch up the verified batches
	   log.Debug("A final proof has been sent, waiting for the network to be synced")

	   	for !a.isSynced(a.ctx, &proofBatchNumberFinal) {
	   		log.Info("Waiting for synchronizer to sync...")
	   		time.Sleep(a.cfg.RetryTime.Duration)
	   	}

	   // network is synced with the final proof, we can safely delete all recursive
	   // proofs up to the last synced batch
	   err = a.State.CleanupGeneratedProofs(a.ctx, proofBatchNumberFinal, nil)

	   	if err != nil {
	   		log.Errorf("Failed to store proof aggregation result: %v", err)
	   	}
	*/
}

/*
func buildMonitoredTxID(batchNumber, batchNumberFinal uint64) string {
	return fmt.Sprintf(monitoredIDFormat, batchNumber, batchNumberFinal)
}
*/

func (a *Aggregator) cleanupLockedProofs() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(a.timeCleanupLockedProofs.Duration):
			n, err := a.state.CleanupLockedProofs(a.ctx, a.cfg.GeneratingProofCleanupThreshold, nil)
			if err != nil {
				log.Errorf("Failed to cleanup locked proofs: %v", err)
			}
			if n == 1 {
				log.Warn("Found a stale proof and removed form cache")
			} else if n > 1 {
				log.Warnf("Found %d stale proofs and removed from cache", n)
			}
		}
	}
}

// FirstToUpper returns the string passed as argument with the first letter in
// uppercase.
func FirstToUpper(s string) string {
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
