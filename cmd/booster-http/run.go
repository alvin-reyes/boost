package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"

	"github.com/filecoin-project/boost/cmd/lib"
	"github.com/filecoin-project/boost/cmd/lib/filters"
	"github.com/filecoin-project/boost/cmd/lib/remoteblockstore"
	"github.com/filecoin-project/boost/metrics"
	"github.com/filecoin-project/boost/piecedirectory"
	bdclient "github.com/filecoin-project/boostd-data/client"
	"github.com/filecoin-project/boostd-data/model"
	"github.com/filecoin-project/boostd-data/shared/tracing"
	"github.com/filecoin-project/dagstore/mount"
	"github.com/filecoin-project/go-state-types/abi"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/markets/dagstore"
	"github.com/ipfs/go-cid"
	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"
)

var runCmd = &cli.Command{
	Name:   "run",
	Usage:  "Start a booster-http process",
	Before: before,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "pprof",
			Usage: "run pprof web server on localhost:6070",
		},
		&cli.StringFlag{
			Name:  "base-path",
			Usage: "the base path at which to run the web server",
			Value: "",
		},
		&cli.StringFlag{
			Name:    "address",
			Aliases: []string{"addr"},
			Usage:   "the listen address for the web server",
			Value:   "0.0.0.0",
		},
		&cli.UintFlag{
			Name:  "port",
			Usage: "the port the web server listens on",
			Value: 7777,
		},
		&cli.StringFlag{
			Name:     "api-lid",
			Usage:    "the endpoint for the local index directory API, eg 'http://localhost:8042'",
			Required: true,
		},
		&cli.IntFlag{
			Name:  "add-index-throttle",
			Usage: "the maximum number of add index operations that can run in parallel",
			Value: 4,
		},
		&cli.StringFlag{
			Name:     "api-fullnode",
			Usage:    "the endpoint for the full node API",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "api-storage",
			Usage:    "the endpoint for the storage node API",
			Required: true,
		},
		&cli.BoolFlag{
			Name:  "serve-pieces",
			Usage: "enables serving raw pieces",
			Value: true,
		},
		&cli.StringFlag{
			Name:  "serve-gateway",
			Usage: "serve deserialized responses with the ipfs gateway API (values: 'verifiable','all','none')",
			Value: "verifiable",
		},
		&cli.BoolFlag{
			Name:  "tracing",
			Usage: "enables tracing of booster-http calls",
			Value: false,
		},
		&cli.StringFlag{
			Name:  "tracing-endpoint",
			Usage: "the endpoint for the tracing exporter",
			Value: "http://tempo:14268/api/traces",
		},
		&cli.StringFlag{
			Name:  "api-filter-endpoint",
			Usage: "the endpoint to use for fetching a remote retrieval configuration for requests",
		},
		&cli.StringFlag{
			Name:  "api-filter-auth",
			Usage: "value to pass in the authorization header when sending a request to the API filter endpoint (e.g. 'Basic ~base64 encoded user/pass~'",
		},
		&cli.StringSliceFlag{
			Name:  "badbits-denylists",
			Usage: "the endpoints for fetching one or more custom BadBits list instead of the default one at https://badbits.dwebops.pub/denylist.json",
			Value: cli.NewStringSlice("https://badbits.dwebops.pub/denylist.json"),
		},
	},
	Action: func(cctx *cli.Context) error {
		servePieces := cctx.Bool("serve-pieces")
		responseFormats := parseSupportedResponseFormats(cctx)
		enableIpfsGateway := len(responseFormats) > 0
		if !servePieces && !enableIpfsGateway {
			return errors.New("one of --serve-pieces, --serve-blocks, etc must be enabled")
		}

		if cctx.Bool("pprof") {
			go func() {
				err := http.ListenAndServe("localhost:6070", nil)
				if err != nil {
					log.Error(err)
				}
			}()
		}

		// Connect to the local index directory service
		ctx := lcli.ReqContext(cctx)
		cl := bdclient.NewStore()
		defer cl.Close(ctx)
		err := cl.Dial(ctx, cctx.String("api-lid"))
		if err != nil {
			return fmt.Errorf("connecting to local index directory service: %w", err)
		}

		// Connect to the full node API
		fnApiInfo := cctx.String("api-fullnode")
		fullnodeApi, ncloser, err := lib.GetFullNodeApi(ctx, fnApiInfo, log)
		if err != nil {
			return fmt.Errorf("getting full node API: %w", err)
		}
		defer ncloser()

		// Connect to the storage API and create a sector accessor
		storageApiInfo := cctx.String("api-storage")
		if err != nil {
			return fmt.Errorf("parsing storage API endpoint: %w", err)
		}

		// Instantiate the tracer and exporter
		enableTracing := cctx.Bool("tracing")
		var tracingStopper func(context.Context) error
		if enableTracing {
			tracingStopper, err = tracing.New("booster-http", cctx.String("tracing-endpoint"))
			if err != nil {
				return fmt.Errorf("failed to instantiate tracer: %w", err)
			}
			log.Info("Tracing exporter enabled")
		}

		// Create the sector accessor
		sa, storageCloser, err := lib.CreateSectorAccessor(ctx, storageApiInfo, fullnodeApi, log)
		if err != nil {
			return err
		}
		defer storageCloser()

		// Create the server API
		pr := &piecedirectory.SectorAccessorAsPieceReader{SectorAccessor: sa}
		pd := piecedirectory.NewPieceDirectory(cl, pr, cctx.Int("add-index-throttle"))

		serveGateway := cctx.String("serve-gateway")
		opts := &HttpServerOptions{
			ServePieces:              servePieces,
			SupportedResponseFormats: responseFormats,
			ServeGateway: serveGateway,
		}
		if enableIpfsGateway {
			repoDir, err := createRepoDir(cctx.String(FlagRepo.Name))
			if err != nil {
				return err
			}

			// Set up badbits filter
			multiFilter := filters.NewMultiFilter(repoDir, cctx.String("api-filter-endpoint"), cctx.String("api-filter-auth"), cctx.StringSlice("badbits-denylists"))
			err = multiFilter.Start(ctx)
			if err != nil {
				return fmt.Errorf("starting block filter: %w", err)
			}

			httpBlockMetrics := remoteblockstore.BlockMetrics{
				GetRequestCount:             metrics.HttpRblsGetRequestCount,
				GetFailResponseCount:        metrics.HttpRblsGetFailResponseCount,
				GetSuccessResponseCount:     metrics.HttpRblsGetSuccessResponseCount,
				BytesSentCount:              metrics.HttpRblsBytesSentCount,
				HasRequestCount:             metrics.HttpRblsHasRequestCount,
				HasFailResponseCount:        metrics.HttpRblsHasFailResponseCount,
				HasSuccessResponseCount:     metrics.HttpRblsHasSuccessResponseCount,
				GetSizeRequestCount:         metrics.HttpRblsGetSizeRequestCount,
				GetSizeFailResponseCount:    metrics.HttpRblsGetSizeFailResponseCount,
				GetSizeSuccessResponseCount: metrics.HttpRblsGetSizeSuccessResponseCount,
			}
			rbs := remoteblockstore.NewRemoteBlockstore(pd, &httpBlockMetrics)
			filtered := filters.NewFilteredBlockstore(rbs, multiFilter)
			opts.Blockstore = filtered
		}
		sapi := serverApi{ctx: ctx, piecedirectory: pd, sa: sa}
		server := NewHttpServer(
			cctx.String("base-path"),
			cctx.String("address"),
			cctx.Int("port"),
			sapi,
			opts,
		)

		// Start the local index directory
		pd.Start(ctx)

		// Start the server
		log.Infof("Starting booster-http node on listen address %s and port %d with base path '%s'",
			cctx.String("address"), cctx.Int("port"), cctx.String("base-path"))
		err = server.Start(ctx)
		if err != nil {
			return fmt.Errorf("starting http server: %w", err)
		}

		log.Infof(ipfsGatewayMsg(cctx, server.ipfsBasePath()))
		if servePieces {
			log.Infof("serving raw pieces at " + server.pieceBasePath())
		} else {
			log.Infof("serving raw pieces is disabled")
		}

		// Monitor for shutdown.
		<-ctx.Done()

		log.Info("Shutting down...")

		err = server.Stop()
		if err != nil {
			return err
		}
		log.Info("Graceful shutdown successful")

		// Sync all loggers.
		_ = log.Sync() //nolint:errcheck

		if enableTracing {
			err = tracingStopper(ctx)
			if err != nil {
				return err
			}
		}

		return nil
	},
}

func parseSupportedResponseFormats(cctx *cli.Context) ([]string) {
	fmts := []string{}
	switch cctx.String("serve-gateway") {
	case "verifiable":
		fmts = append(fmts, "application/vnd.ipld.raw") // raw
		fmts = append(fmts, "application/vnd.ipld.car") // car
	case "all":
		fmts = append(fmts, "")
	}
	return fmts
}

func ipfsGatewayMsg(cctx *cli.Context, ipfsBasePath string) string {
	fmts := []string{}
	switch cctx.String("serve-gateway") {
	case "verifiable":
		fmts = append(fmts, "blocks")
		fmts = append(fmts, "CARs")
	case "all":
		fmts = append(fmts, "files")
	}

	if len(fmts) == 0 {
		return "IPFS gateway is disabled"
	}

	return "serving IPFS gateway at " + ipfsBasePath + " (serving " + strings.Join(fmts, ", ") + ")"
}

func createRepoDir(repoDir string) (string, error) {
	repoDir, err := homedir.Expand(repoDir)
	if err != nil {
		return "", fmt.Errorf("expanding repo file path: %w", err)
	}
	if repoDir == "" {
		return "", fmt.Errorf("%s is a required flag", FlagRepo.Name)
	}
	return repoDir, os.MkdirAll(repoDir, 0744)
}

type serverApi struct {
	ctx            context.Context
	piecedirectory *piecedirectory.PieceDirectory
	sa             dagstore.SectorAccessor
}

var _ HttpServerApi = (*serverApi)(nil)

func (s serverApi) GetPieceDeals(ctx context.Context, pieceCID cid.Cid) ([]model.DealInfo, error) {
	return s.piecedirectory.GetPieceDeals(ctx, pieceCID)
}

func (s serverApi) IsUnsealed(ctx context.Context, sectorID abi.SectorNumber, offset abi.UnpaddedPieceSize, length abi.UnpaddedPieceSize) (bool, error) {
	return s.sa.IsUnsealed(ctx, sectorID, offset, length)
}

func (s serverApi) UnsealSectorAt(ctx context.Context, sectorID abi.SectorNumber, offset abi.UnpaddedPieceSize, length abi.UnpaddedPieceSize) (mount.Reader, error) {
	return s.sa.UnsealSectorAt(ctx, sectorID, offset, length)
}
