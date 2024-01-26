package types

import "github.com/0xPolygonHermez/zkevm-aggregator/aggregator/prover"

// FinalProofInputs struct
type FinalProofInputs struct {
	FinalProof       *prover.FinalProof
	NewLocalExitRoot []byte
	NewStateRoot     []byte
}
