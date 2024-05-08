package state

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// FORKID_BLUEBERRY is the fork id 4
	FORKID_BLUEBERRY = 4
	// FORKID_DRAGONFRUIT is the fork id 5
	FORKID_DRAGONFRUIT = 5
	// FORKID_INCABERRY is the fork id 6
	FORKID_INCABERRY = 6
	// FORKID_ETROG is the fork id 7
	FORKID_ETROG = 7
	// FORKID_ELDERBERRY is the fork id 8
	FORKID_ELDERBERRY = 8
	// FORKID_ELDERBERRY_2 is the fork id 9
	FORKID_ELDERBERRY_2 = 9
)

// Batch struct
type Batch struct {
	BatchNumber     uint64
	Coinbase        common.Address
	BatchL2Data     []byte
	StateRoot       common.Hash
	LocalExitRoot   common.Hash
	AccInputHash    common.Hash
	L1InfoTreeIndex uint32
	L1InfoRoot      common.Hash
	// Timestamp (<=incaberry) -> batch time
	// 			 (>incaberry) -> minTimestamp used in batch creation, real timestamp is in virtual_batch.batch_timestamp
	Timestamp      time.Time
	Transactions   []types.Transaction
	GlobalExitRoot common.Hash
	ForcedBatchNum *uint64
	ForkID         uint64
}

// Sequence represents the sequence interval
type Sequence struct {
	FromBatchNumber uint64
	ToBatchNumber   uint64
}

// Proof struct
type Proof struct {
	BatchNumber      uint64
	BatchNumberFinal uint64
	Proof            string
	InputProver      string
	ProofID          *string
	// Prover name, unique identifier across prover reboots.
	Prover *string
	// ProverID prover process identifier.
	ProverID *string
	// GeneratingSince holds the timestamp for the moment in which the
	// proof generation has started by a prover. Nil if the proof is not
	// currently generating.
	GeneratingSince *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
