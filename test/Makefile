DOCKERCOMPOSE := docker-compose -f docker-compose.yml
DOCKERCOMPOSEAPPAGG := zkevm-aggregator
DOCKERCOMPOSEAGGDB := zkevm-aggregator-db

RUNAGGDB := $(DOCKERCOMPOSE) up -d $(DOCKERCOMPOSESTATEDB)
RUNAGGREGATOR := $(DOCKERCOMPOSE) up -d $(DOCKERCOMPOSEAPPAGG)

RUNMETRICS := $(DOCKERCOMPOSE) up -d $(DOCKERCOMPOSEMETRICS)

RUN := $(DOCKERCOMPOSE) up -d

STOPAGGDB := $(DOCKERCOMPOSE) stop $(DOCKERCOMPOSEAGGDB) && $(DOCKERCOMPOSE) rm -f $(DOCKERCOMPOSEAGGDB)
STOPAGGREGATOR := $(DOCKERCOMPOSE) stop $(DOCKERCOMPOSEAPPAGG) && $(DOCKERCOMPOSE) rm -f $(DOCKERCOMPOSEAPPAGG)

STOP := $(DOCKERCOMPOSE) down --remove-orphans

.PHONY: run-db
run-db: ## Runs the node database
	$(RUNAGGDB)
	
.PHONY: stop-db
stop-db: ## Stops the node database
	$(STOPAGGDB)

.PHONY: run-agg
run-agg: ## Runs the aggregator service
	$(RUNAGGREGATOR)

.PHONY: stop-agg
stop-agg: ## Stops the aggregator service
	$(STOPAGGREGATOR)

.PHONY: run
run: ## Runs a full node
	$(RUNAGGDB)
	sleep 1
	$(RUNAGGREGATOR)

.PHONY: stop
stop: ## Stops all services
	$(STOP)

.PHONY: ship
ship: ## Builds docker images and run them
	cd .. && make build-docker && cd ./test && make run

.PHONY: reship
reship: stop ship ## Executes `make stop` and `make sail` commands

.PHONY: restart
restart: stop run ## Executes `make stop` and `make run` commands

.PHONY: show-logs
show-logs:  ## Show logs for running docker 
	$(DOCKERCOMPOSE) logs

.PHONY: install-mockery
install-mockery: ## Installs mockery with the correct version to generate the mocks
	go install github.com/vektra/mockery/v2@v2.39.0

.PHONY: generate-mocks
generate-mocks: generate-mocks-aggregator ## Generates mocks for the tests, using mockery tool

.PHONY: generate-mocks-aggregator	
generate-mocks-aggregator: ## Generates mocks for aggregator , using mockery tool
	## mocks for the aggregator tests
	export "GOROOT=$$(go env GOROOT)" && $$(go env GOPATH)/bin/mockery --name=stateInterface --dir=../aggregator --output=../aggregator/mocks --outpkg=mocks --structname=StateMock --filename=mock_state.go
	export "GOROOT=$$(go env GOROOT)" && $$(go env GOPATH)/bin/mockery --name=proverInterface --dir=../aggregator --output=../aggregator/mocks --outpkg=mocks --structname=ProverMock --filename=mock_prover.go
	export "GOROOT=$$(go env GOROOT)" && $$(go env GOPATH)/bin/mockery --name=etherman --dir=../aggregator --output=../aggregator/mocks --outpkg=mocks --structname=Etherman --filename=mock_etherman.go
	export "GOROOT=$$(go env GOROOT)" && $$(go env GOPATH)/bin/mockery --name=ethTxManager --dir=../aggregator --output=../aggregator/mocks --outpkg=mocks --structname=EthTxManager --filename=mock_ethtxmanager.go
	export "GOROOT=$$(go env GOROOT)" && $$(go env GOPATH)/bin/mockery --name=aggregatorTxProfitabilityChecker --dir=../aggregator --output=../aggregator/mocks --outpkg=mocks --structname=ProfitabilityCheckerMock --filename=mock_profitabilitychecker.go
	export "GOROOT=$$(go env GOROOT)" && $$(go env GOPATH)/bin/mockery --name=Tx --srcpkg=github.com/jackc/pgx/v4 --output=../aggregator/mocks --outpkg=mocks --structname=DbTxMock --filename=mock_dbtx.go

## Help display.
## Pulls comments from beside commands and prints a nicely formatted
## display with the commands and their usage information.
.DEFAULT_GOAL := help

.PHONY: help
help: ## Prints this help
		@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
