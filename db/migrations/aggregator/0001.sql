-- +migrate Down
DROP SCHEMA IF EXISTS aggregator CASCADE;

-- +migrate Up
CREATE SCHEMA aggregator;

CREATE TABLE aggregator.proof (
	batch_num int8 NOT NULL,
	batch_num_final int8 NOT NULL,
	proof varchar NULL,
	proof_id varchar NULL,
	input_prover varchar NULL,
	prover varchar NULL,
	prover_id varchar NULL,
	created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
	updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
	generating_since timestamptz NULL,
    PRIMARY KEY (batch_num, batch_num_final)
);

