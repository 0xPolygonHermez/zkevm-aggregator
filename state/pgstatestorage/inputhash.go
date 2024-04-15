package pgstatestorage

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
)

// AddAccInputHash stores an AccInputHash
func (p *PostgresStorage) AddAccInputHash(ctx context.Context, batchNumber uint64, accInputHash common.Hash, dbTx pgx.Tx) error {
	const addInputHashSQL = `
		INSERT INTO aggregator.acc_input_hash (batch_num, hash) VALUES ($1, $2) ON CONFLICT (batch_num) DO NOTHING;
		`
	e := p.getExecQuerier(dbTx)
	_, err := e.Exec(ctx, addInputHashSQL, batchNumber, accInputHash.String())
	return err
}

// GetAccInputHash gets an AccInputHash for a given batch number
func (p *PostgresStorage) GetAccInputHash(ctx context.Context, batchNumber uint64, dbTx pgx.Tx) (common.Hash, error) {
	const getInputHashSQL = `SELECT hash FROM aggregator.acc_input_hash WHERE batch_num = $1`
	e := p.getExecQuerier(dbTx)
	var hashStr string
	err := e.QueryRow(ctx, getInputHashSQL, batchNumber).Scan(&hashStr)
	if err != nil {
		return common.Hash{}, err
	}
	return common.HexToHash(hashStr), nil
}

// DeleteAccInputHashesOlderThanBatchNumber deletes hashes previous to the given batch number
func (p *PostgresStorage) DeleteAccInputHashesOlderThanBatchNumber(ctx context.Context, batchNumber uint64, dbTx pgx.Tx) error {
	const deleteGeneratedProofSQL = "DELETE FROM aggregator.acc_input_hash WHERE batch_num < $1"
	e := p.getExecQuerier(dbTx)
	_, err := e.Exec(ctx, deleteGeneratedProofSQL, batchNumber)
	return err
}
