package main

import (
	"os"

	"github.com/0xPolygonHermez/zkevm-aggregator"
	"github.com/urfave/cli/v2"
)

func versionCmd(*cli.Context) error {
	zkevm.PrintVersion(os.Stdout)
	return nil
}
