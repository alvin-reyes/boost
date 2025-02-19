package main

import (
	"fmt"

	bcli "github.com/filecoin-project/boost/cli"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/urfave/cli/v2"
)

var indexProvCmd = &cli.Command{
	Name:  "index",
	Usage: "Manage the index provider on Boost",
	Subcommands: []*cli.Command{
		indexProvAnnounceAllCmd,
		indexProvListMultihashesCmd,
		indexProvAnnounceLatest,
		indexProvAnnounceLatestHttp,
		indexProvAnnounceDealRemovalAd,
	},
}

var indexProvAnnounceAllCmd = &cli.Command{
	Name:  "announce-all",
	Usage: "Announce all active deals to indexers so they can download the indices",
	Flags: []cli.Flag{},
	Action: func(cctx *cli.Context) error {
		ctx := lcli.ReqContext(cctx)

		// get boost api
		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		// announce markets and boost deals
		return napi.BoostIndexerAnnounceAllDeals(ctx)
	},
}

var indexProvListMultihashesCmd = &cli.Command{
	Name:      "list-multihashes",
	Usage:     "list-multihashes <proposal cid>",
	UsageText: "List multihashes for a deal by proposal cid",
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() != 1 {
			return fmt.Errorf("must supply proposal cid")
		}

		propCid, err := cid.Parse(cctx.Args().First())
		if err != nil {
			return fmt.Errorf("parsing proposal cid %s: %w", cctx.Args().First(), err)
		}

		ctx := lcli.ReqContext(cctx)

		// get boost api
		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		// get list of multihashes
		mhs, err := napi.BoostIndexerListMultihashes(ctx, propCid)
		if err != nil {
			return err
		}

		fmt.Printf("Found %d multihashes for deal with proposal cid %s:\n", len(mhs), propCid)
		for _, mh := range mhs {
			fmt.Println("  " + mh.String())
		}

		return nil
	},
}

var indexProvAnnounceLatest = &cli.Command{
	Name:  "announce-latest",
	Usage: "Re-publish the latest existing advertisement to pubsub",
	Action: func(cctx *cli.Context) error {
		ctx := lcli.ReqContext(cctx)

		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		c, err := napi.BoostIndexerAnnounceLatest(ctx)
		if err != nil {
			return err
		}

		fmt.Printf("Announced advertisement with cid %s\n", c)
		return nil
	},
}

var indexProvAnnounceLatestHttp = &cli.Command{
	Name:  "announce-latest-http",
	Usage: "Re-publish the latest existing advertisement to specific indexers over http",
	Flags: []cli.Flag{
		&cli.StringSliceFlag{
			Name:     "announce-url",
			Usage:    "The url(s) to announce to. If not specified, announces to the http urls in config",
			Required: false,
		},
	},
	Action: func(cctx *cli.Context) error {
		ctx := lcli.ReqContext(cctx)

		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		c, err := napi.BoostIndexerAnnounceLatestHttp(ctx, cctx.StringSlice("announce-url"))
		if err != nil {
			return err
		}

		fmt.Printf("Announced advertisement to indexers over http with cid %s\n", c)
		return nil
	},
}

var indexProvAnnounceDealRemovalAd = &cli.Command{
	Name:  "announce-remove-deal",
	Usage: "Published a removal ad for given deal UUID or Proposal CID (legacy deals)",
	Action: func(cctx *cli.Context) error {
		ctx := lcli.ReqContext(cctx)

		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		if cctx.Args().Len() != 1 {
			return fmt.Errorf("must specify only one proposal CID / deal UUID")
		}

		id := cctx.Args().Get(0)

		var proposalCid *cid.Cid
		dealUuid, err := uuid.Parse(id)
		if err != nil {
			propCid, err := cid.Decode(id)
			if err != nil {
				return fmt.Errorf("could not parse '%s' as deal uuid or proposal cid", id)
			}
			proposalCid = &propCid
		}

		if proposalCid == nil {
			deal, err := napi.BoostDeal(ctx, dealUuid)
			if err != nil {
				return fmt.Errorf("deal not found with UUID %s: %w", dealUuid.String(), err)
			}
			prop, err := deal.ClientDealProposal.Proposal.Cid()
			if err != nil {
				return fmt.Errorf("generating proposal cid for deal %s: %w", dealUuid.String(), err)
			}
			proposalCid = &prop
		} else {
			_, err = napi.BoostLegacyDealByProposalCid(ctx, *proposalCid)
			if err != nil {
				_, err := napi.BoostDealBySignedProposalCid(ctx, *proposalCid)
				if err != nil {
					return fmt.Errorf("no deal with proposal CID %s found in boost and legacy database", proposalCid.String())
				}
			}
		}

		cid, err := napi.BoostIndexerAnnounceDealRemoved(ctx, *proposalCid)
		if err != nil {
			return fmt.Errorf("failed to send removal ad: %w", err)
		}
		fmt.Printf("Announced the removal Ad with cid %s\n", cid)

		return nil
	},
}
